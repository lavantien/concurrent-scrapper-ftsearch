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

type InputType string

const (
	SearchQuery InputType = "searchQuery"
	URLInput    InputType = "urlInput"
)

type Model struct {
	db            *sql.DB
	urlQueue      URLQueue
	results       chan FetchResult
	wg            sync.WaitGroup
	sem           Semaphore
	numWorkers    int
	activeWorkers int
	urlInput      textinput.Model
	searchInput   textinput.Model
	focusedInput  InputType // Track which input field is focused
	searchResults []string
	status        string
}

type FetchResult struct {
	URL   string
	Body  []byte
	Error error
}

type (
	URLQueue  chan string
	Semaphore chan struct{}
	errMsg    error
)

func (s Semaphore) Acquire() {
	s <- struct{}{}
}

func (s Semaphore) Release() {
	<-s
}

// Check if the focused input is the search query.
func (m *Model) IsSearchQueryFocused() bool {
	return m.focusedInput == SearchQuery
}

func initDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
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
		return nil, fmt.Errorf("failed to create URL table: %v", err)
	}

	// Create FTS5 content table
	_, err = db.Exec(`
        CREATE VIRTUAL TABLE IF NOT EXISTS content USING fts5(
            url_id,
            body
        )
    `)
	if err != nil {
		return nil, fmt.Errorf("failed to create FTS5 content table: %v", err)
	}

	return db, nil
}

func fetcherWorker(queue URLQueue, results chan<- FetchResult, wg *sync.WaitGroup, activeWorkers *int) {
	for url := range queue {
		*activeWorkers++
		resp, err := http.Get(url)
		if err != nil {
			results <- FetchResult{URL: url, Error: fmt.Errorf("failed to fetch URL %s: %v", url, err)}
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close() // Close the response body explicitly
		if err != nil {
			results <- FetchResult{URL: url, Error: fmt.Errorf("failed to read response body for URL %s: %v", url, err)}
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
		return "", fmt.Errorf("failed to parse HTML: %v", err)
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
		return false, fmt.Errorf("failed to parse URL: %v", err)
	}

	robotsURL := fmt.Sprintf("%s://%s/robots.txt", u.Scheme, u.Host)
	resp, err := http.Get(robotsURL)
	if err != nil {
		return false, fmt.Errorf("failed to fetch robots.txt: %v", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("failed to read robots.txt response body: %v", err)
	}

	group, err := robotstxt.FromBytes(data)
	if err != nil {
		return false, fmt.Errorf("failed to parse robots.txt: %v", err)
	}

	return group.TestAgent(u.Path, "MyCrawler"), nil
}

func saveToDB(db *sql.DB, url string, body string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}

	// Insert URL into urls table
	result, err := tx.Exec(`
        INSERT OR IGNORE INTO urls (url) VALUES (?)
    `, url)
	if err != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil {
			return fmt.Errorf("failed to insert URL and roll back transaction: %v", err)
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
			return fmt.Errorf("failed to insert content and roll back transaction: %v", err)
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
		return nil, fmt.Errorf("failed to execute search query: %v", err)
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, fmt.Errorf("failed to scan result row: %v", err)
		}
		results = append(results, url)
	}

	return results, nil
}

func initModel(dbPath string) (*Model, error) {
	db, err := initDB(dbPath)
	if err != nil {
		fmt.Printf("fail to init db %v\n", err)
		return nil, fmt.Errorf("failed to initialize database: %v", err)
	}

	urlQueue := make(URLQueue, 100)
	results := make(chan FetchResult, 100)

	numWorkers := 10
	sem := make(Semaphore, numWorkers)

	tiURL := textinput.New()
	tiURL.Placeholder = "Enter URL"
	tiURL.Focus()
	tiURL.CharLimit = 256
	tiURL.Width = 100
	tiSearch := textinput.New()
	tiSearch.Placeholder = "Enter keyword"
	tiSearch.CharLimit = 256
	tiSearch.Width = 100

	model := &Model{
		db:            db,
		urlQueue:      urlQueue,
		results:       results,
		wg:            sync.WaitGroup{},
		sem:           sem,
		numWorkers:    numWorkers,
		activeWorkers: 0,
		urlInput:      tiURL,
		searchInput:   tiSearch, // Initialize the search input field
		focusedInput:  URLInput,
		searchResults: []string{},
		status:        "Initializing...",
	}

	// Start fetcher workers
	for i := 0; i < numWorkers; i++ {
		model.wg.Add(1)
		go func() {
			defer model.wg.Done()
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
				fmt.Println("error", result.URL, result.Error)
				continue
			}

			textContent, err := parseHTML(result.Body)
			if err != nil {
				model.status = fmt.Sprintf("Error parsing %s: %v", result.URL, err)
				fmt.Println("fail to parse html", result.URL, err)
				continue
			}

			allowed, err := isAllowed(result.URL)
			if err != nil {
				model.status = fmt.Sprintf("Error checking robots.txt for %s: %v\n", result.URL, err)
				fmt.Println("fail to check robots", result.URL, err)
				continue
			}

			if allowed {
				err := saveToDB(model.db, result.URL, textContent)
				if err != nil {
					model.status = fmt.Sprintf("Error saving %s to database: %v", result.URL, err)
					fmt.Println("fail to save db", result.URL, err)
				}
			}
		}

		close(results) // Close results channel after all fetcher workers have finished
	}()

	return model, nil
}

func (m *Model) Init() tea.Cmd {
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	var url, keyword string

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.wg.Wait()
			close(m.urlQueue)
			return m, tea.Quit
		case tea.KeyEnter:
			// Only do action when Enter is pressed
			if m.focusedInput == SearchQuery {
				m.searchInput.Focus()
				keyword = m.searchInput.Value()
				if len(keyword) > 0 {
					results, err := searchDB(m.db, keyword)
					if err != nil {
						m.status = fmt.Sprintf("Search error: %v", err)
					} else {
						m.searchResults = results
						m.status = fmt.Sprintf("Found %d results for '%s'", len(results), keyword)
					}
				}
			} else if m.focusedInput == URLInput {
				m.urlInput.Focus()
				url = m.urlInput.Value()
				if len(url) > 0 {
					m.urlQueue <- url
					m.status = "URL added to queue"
				}
			}
		case tea.KeyTab:
			// Toggle focus between input fields
			if m.focusedInput == SearchQuery {
				m.focusedInput = URLInput
			} else {
				m.focusedInput = SearchQuery
			}
		}

	case errMsg:
		fmt.Printf("error on input %v\n", msg)
		return m, nil
	}

	// Update search query model
	m.searchInput, _ = m.searchInput.Update(msg)

	// Update url input model
	m.urlInput, cmd = m.urlInput.Update(msg)

	return m, cmd
}

func (m *Model) View() string {
	var b strings.Builder

	b.WriteString("Web Crawler TUI\n")

	// URL Input Field
	if m.IsSearchQueryFocused() {
		b.WriteString("\nEnter keyword to search:\n")
		b.WriteString(m.searchInput.View())
	} else {
		b.WriteString("\nEnter URL to crawl:\n")
		b.WriteString(m.urlInput.View())
	}

	// Crawl Status
	b.WriteString("\nCrawl Status:\n")
	b.WriteString(fmt.Sprintf("Active Workers: %d/%d\n", m.activeWorkers, m.numWorkers))
	b.WriteString(m.status + "\n")

	// Search Results
	b.WriteString("\nSearch Results:\n")
	for _, result := range m.searchResults {
		b.WriteString(result + "\n")
	}

	// Debugging Information
	b.WriteString("\nDebugging Information:\n")
	b.WriteString(fmt.Sprintf("URL Queue Length: %d\n", len(m.urlQueue)))
	b.WriteString(fmt.Sprintf("Results Channel Length: %d\n", len(m.results)))

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
	p := tea.NewProgram(model)
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running program: %v\n", err)
		return
	}
}
