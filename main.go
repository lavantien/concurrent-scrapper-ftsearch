package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	_ "github.com/mattn/go-sqlite3"
	"github.com/temoto/robotstxt"
	"golang.org/x/net/html"
)

type Model struct {
	db            *sql.DB
	urlQueue      URLQueue
	results       chan FetchResult
	wg            sync.WaitGroup
	sem           Semaphore
	numWorkers    int
	activeWorkers int
	searchQuery   textinput.Model
	searchResults []string
	status        string
}

type FetchResult struct {
	URL   string
	Body  []byte
	Error error
}

type URLQueue chan string

type Semaphore chan struct{}

func (s Semaphore) Acquire() {
	s <- struct{}{}
}

func (s Semaphore) Release() {
	<-s
}

func initDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Create URL table
	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS urls (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            url TEXT UNIQUE NOT NULL,
            fetched_at DATETIME DEFAULT CURRENT_TIMESTAMP
        )
    `)
	if err != nil {
		return nil, err
	}

	// Create FTS5 content table
	_, err = db.Exec(`
        CREATE VIRTUAL TABLE IF NOT EXISTS content USING fts5(
            url_id INTEGER,
            body TEXT
        )
    `)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func fetcherWorker(queue URLQueue, results chan<- FetchResult, wg *sync.WaitGroup, activeWorkers *int) {
	for url := range queue {
		*activeWorkers++
		resp, err := http.Get(url)
		if err != nil {
			results <- FetchResult{URL: url, Error: err}
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close() // Close the response body explicitly
		if err != nil {
			results <- FetchResult{URL: url, Error: err}
			continue
		}
		results <- FetchResult{URL: url, Body: body}
		*activeWorkers--
	}
	wg.Done()
}

func parseHTML(body []byte) (string, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	var textContent string
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			textContent += n.Data + " "
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
	return textContent, nil
}

func isAllowed(_url string) (bool, error) {
	u, err := url.Parse(_url)
	if err != nil {
		return false, err
	}
	robotsURL := fmt.Sprintf("%s://%s/robots.txt", u.Scheme, u.Host)
	resp, err := http.Get(robotsURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	group, err := robotstxt.FromBytes(data)
	if err != nil {
		return false, err
	}
	return group.TestAgent(u.Path, "MyCrawler"), nil
}

func saveToDB(db *sql.DB, url string, body string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}

	// Insert URL into urls table
	result, err := tx.Exec(`
        INSERT OR IGNORE INTO urls (url) VALUES (?)
    `, url)
	if err != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil {
			return fmt.Errorf("failed to insert URL into database and roll back transaction: %v", err)
		}
		return fmt.Errorf("failed to insert URL into database: %v", err)
	}
	urlID, err := result.LastInsertId()
	if err != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil {
			return fmt.Errorf("failed to get URL ID and roll back transaction: %v", err)
		}
		return fmt.Errorf("failed to get URL ID: %v", err)
	}

	// Insert content into FTS5 table
	_, err = tx.Exec(`
        INSERT INTO content (url_id, body) VALUES (?, ?)
    `, urlID, body)
	if err != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil {
			return fmt.Errorf("failed to insert content into database and roll back transaction: %v", err)
		}
		return fmt.Errorf("failed to insert content into database: %v", err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit transaction: %v", err)
	}

	return nil
}

func searchDB(db *sql.DB, keyword string) ([]string, error) {
	rows, err := db.Query(`
        SELECT T1.url FROM urls AS T1 JOIN content AS T2 ON T1.id = T2.url_id WHERE T2.body MATCH ?
    `, keyword)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, err
		}
		results = append(results, url)
	}

	return results, nil
}

func initModel(dbPath string) (*Model, error) {
	db, err := initDB(dbPath)
	if err != nil {
		return nil, err
	}

	urlQueue := make(URLQueue, 100)
	results := make(chan FetchResult, 100)

	numWorkers := 10
	sem := make(Semaphore, numWorkers)

	model := &Model{
		db:            db,
		urlQueue:      urlQueue,
		results:       results,
		wg:            sync.WaitGroup{},
		sem:           sem,
		numWorkers:    numWorkers,
		activeWorkers: 0,
		searchQuery:   textinput.New(),
		searchResults: []string{},
		status:        "Initializing...",
	}

	// Start fetcher workers
	for i := 0; i < numWorkers; i++ {
		model.wg.Add(1)
		go func() {
			model.sem.Acquire()
			defer model.sem.Release()
			fetcherWorker(model.urlQueue, model.results, &model.wg, &model.activeWorkers)
		}()
	}

	// Start parser and saver
	go func() {
		for result := range model.results {
			if result.Error != nil {
				model.status = fmt.Sprintf("Error fetching %s: %v", result.URL, result.Error)
				continue
			}
			textContent, err := parseHTML(result.Body)
			if err != nil {
				model.status = fmt.Sprintf("Error parsing %s: %v", result.URL, err)
				continue
			}
			allowed, err := isAllowed(result.URL)
			if err != nil {
				model.status = fmt.Sprintf("Error checking robots.txt for %s: %v\n", result.URL, err)
				continue
			}
			if allowed {
				model.urlQueue <- result.URL
				err := saveToDB(model.db, result.URL, textContent)
				if err != nil {
					model.status = fmt.Sprintf("Error saving %s to database: %v", result.URL, err)
				}
			}
		}
		close(results) // Close results channel after all fetcher workers have finished
	}()

	return model, nil
}

func (m *Model) Init() tea.Cmd {
	return textinput.Blink
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.wg.Wait()
			close(m.urlQueue)
			return m, tea.Quit
		case "enter":
			if len(m.searchQuery.Value()) > 0 {
				results, err := searchDB(m.db, m.searchQuery.Value())
				if err != nil {
					m.status = fmt.Sprintf("Search error: %v", err)
				} else {
					m.searchResults = results
					m.status = fmt.Sprintf("Found %d results for '%s'", len(results), m.searchQuery.Value())
				}
			}
		default:
			m.searchQuery, cmd = m.searchQuery.Update(msg)
		}
	}

	return m, cmd
}

func (m *Model) View() string {
	var b strings.Builder

	b.WriteString("Web Crawler TUI\n")
	b.WriteString(fmt.Sprintf("Status: %s\n", m.status))
	b.WriteString(fmt.Sprintf("Active Workers: %d/%d\n", m.activeWorkers, m.numWorkers))
	b.WriteString("\nEnter URL to crawl or search keyword:\n")
	b.WriteString(m.searchQuery.View())
	b.WriteString("\nSearch Results:\n")

	for _, result := range m.searchResults {
		b.WriteString(result + "\n")
	}

	return b.String()
}

func main() {
	dbPath := "crawler.db"
	model, err := initModel(dbPath)
	if err != nil {
		panic(err)
	}
	defer model.db.Close()

	// Seed the queue with initial URLs
	initialURLs := []string{"https://example.com"}
	for _, url := range initialURLs {
		model.urlQueue <- url
	}

	// Start Bubble Tea application
	if model, err := tea.NewProgram(model).Run(); err != nil {
		fmt.Printf("Error running program: %v\n%v\n", err, model)
		return
	}
}
