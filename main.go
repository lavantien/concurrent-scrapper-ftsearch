package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	_ "github.com/mattn/go-sqlite3"
	"github.com/temoto/robotstxt"
	"golang.org/x/net/html"
)

type InputType string

var (
	focusedStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	blurredStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cursorStyle         = focusedStyle
	noStyle             = lipgloss.NewStyle()
	helpStyle           = blurredStyle
	cursorModeHelpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	focusedButton = focusedStyle.Render("[ Submit ]")
	blurredButton = fmt.Sprintf("[ %s ]", blurredStyle.Render("Submit"))
)

type WorkerLog struct {
	WorkerID    int
	URL         string
	ProcessTime time.Duration
	StartTime   time.Time
}

type Model struct {
	db       *sql.DB
	urlQueue chan string
	results  chan FetchResult
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	inputs     []textinput.Model
	focusIndex int
	cursorMode cursor.Mode

	searchResults []string
	status        string
	workerLogs    []WorkerLog

	activeWorkers int32 // Use atomic for thread-safe counter
	maxWorkers    int
}

type FetchResult struct {
	URL         string
	Body        []byte
	Error       error
	WorkerID    int
	ProcessTime time.Duration
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
	ctx, cancel := context.WithCancel(context.Background())

	db, err := initDB(dbPath)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to initialize database: %v", err)
	}

	// Recreate input fields with more detailed configuration
	inputs := make([]textinput.Model, 2)
	for i := range inputs {
		t := textinput.New()
		t.Cursor.Style = cursorStyle
		t.CharLimit = 256

		switch i {
		case 0:
			t.Placeholder = "Enter URL"
			t.Focus()
			t.PromptStyle = focusedStyle
			t.TextStyle = focusedStyle
		case 1:
			t.Placeholder = "Enter keyword"
			t.Blur()
			t.PromptStyle = noStyle
			t.TextStyle = noStyle
		}

		inputs[i] = t
	}

	model := &Model{
		db:       db,
		urlQueue: make(chan string, 100),
		results:  make(chan FetchResult, 100),
		ctx:      ctx,
		cancel:   cancel,

		inputs:     inputs,
		focusIndex: 0,
		cursorMode: cursor.CursorBlink,

		searchResults: []string{},
		status:        "Ready",
		activeWorkers: 0,
		maxWorkers:    10,
		workerLogs:    []WorkerLog{},
	}

	// Start fetcher workers
	for i := 0; i < model.maxWorkers; i++ {
		model.wg.Add(1)
		go model.fetcherWorker(i + 1) // Use 1-based worker IDs
	}

	// Start result processor
	go model.processResults()

	return model, nil
}

func (m *Model) fetcherWorker(workerID int) {
	defer m.wg.Done()

	for {
		select {
		case <-m.ctx.Done():
			return
		case url, ok := <-m.urlQueue:
			if !ok {
				return
			}

			atomic.AddInt32(&m.activeWorkers, 1)
			result := m.fetchAndProcessURL(workerID, url)
			m.results <- result
			atomic.AddInt32(&m.activeWorkers, -1)
		}
	}
}

func (m *Model) fetchAndProcessURL(workerID int, urlStr string) FetchResult {
	startTime := time.Now()

	allowed, err := isAllowed(urlStr)
	if err != nil {
		return FetchResult{
			URL:         urlStr,
			Error:       err,
			WorkerID:    workerID,
			ProcessTime: time.Since(startTime),
		}
	}

	if !allowed {
		return FetchResult{
			URL:         urlStr,
			Error:       fmt.Errorf("not allowed by robots.txt"),
			WorkerID:    workerID,
			ProcessTime: time.Since(startTime),
		}
	}

	// Simulate processing complexity
	time.Sleep(500 * time.Millisecond)

	resp, err := http.Get(urlStr)
	if err != nil {
		return FetchResult{
			URL:         urlStr,
			Error:       err,
			WorkerID:    workerID,
			ProcessTime: time.Since(startTime),
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return FetchResult{
			URL:         urlStr,
			Error:       err,
			WorkerID:    workerID,
			ProcessTime: time.Since(startTime),
		}
	}

	return FetchResult{
		URL:         urlStr,
		Body:        body,
		WorkerID:    workerID,
		ProcessTime: time.Since(startTime),
	}
}

func (m *Model) processResults() {
	for result := range m.results {
		// Log worker performance
		workerLog := WorkerLog{
			WorkerID:    result.WorkerID,
			URL:         result.URL,
			ProcessTime: result.ProcessTime,
			StartTime:   time.Now(),
		}
		m.workerLogs = append(m.workerLogs, workerLog)

		// Limit log size to prevent memory growth
		if len(m.workerLogs) > 50 {
			m.workerLogs = m.workerLogs[len(m.workerLogs)-50:]
		}

		if result.Error != nil {
			m.status = fmt.Sprintf("Worker %d Error: %s", result.WorkerID, result.Error)
			continue
		}

		textContent, err := parseHTML(result.Body)
		if err != nil {
			m.status = fmt.Sprintf("Worker %d Parse Error: %v", result.WorkerID, err)
			continue
		}

		err = saveToDB(m.db, result.URL, textContent)
		if err != nil {
			m.status = fmt.Sprintf("Worker %d DB Save Error: %v", result.WorkerID, err)
		} else {
			m.status = fmt.Sprintf("Worker %d processed %s in %v",
				result.WorkerID, result.URL, result.ProcessTime)
		}
	}
}

func (m *Model) Init() tea.Cmd {
	// Restore input initialization logic
	cmds := make([]tea.Cmd, len(m.inputs))
	for i := range m.inputs {
		if i == m.focusIndex {
			cmds[i] = m.inputs[i].Focus()
			m.inputs[i].PromptStyle = focusedStyle
			m.inputs[i].TextStyle = focusedStyle
		} else {
			m.inputs[i].Blur()
			m.inputs[i].PromptStyle = noStyle
			m.inputs[i].TextStyle = noStyle
		}
		cmds[i] = m.inputs[i].Cursor.SetMode(m.cursorMode)
	}

	return tea.Batch(cmds...)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.cancel()
			m.wg.Wait()
			close(m.urlQueue)
			close(m.results)
			return m, tea.Quit

		case "tab", "shift+tab", "up", "down":
			s := msg.String()

			if s == "up" || s == "shift+tab" {
				m.focusIndex--
			} else {
				m.focusIndex++
			}

			// Wrap focus index
			if m.focusIndex > len(m.inputs) {
				m.focusIndex = 0
			} else if m.focusIndex < 0 {
				m.focusIndex = len(m.inputs)
			}

			return m, m.updateInputs(msg)

		case "enter":
			if m.focusIndex == 0 {
				// URL Input
				url := m.inputs[0].Value()
				if url != "" {
					m.urlQueue <- url
					m.status = fmt.Sprintf("Added URL: %s", url)
					m.inputs[0].Reset()
				}
			} else if m.focusIndex == 1 {
				// Search Input
				keyword := m.inputs[1].Value()
				if keyword != "" {
					results, err := searchDB(m.db, keyword)
					if err != nil {
						m.status = fmt.Sprintf("Search error: %v", err)
					} else {
						m.searchResults = results
						m.status = fmt.Sprintf("Found %d results", len(results))
					}
					m.inputs[1].Reset()
				}
			}

		case "ctrl+r":
			// Cycle cursor mode
			m.cursorMode++
			if m.cursorMode > cursor.CursorHide {
				m.cursorMode = cursor.CursorBlink
			}

			cmds := make([]tea.Cmd, len(m.inputs))
			for i := range m.inputs {
				cmds[i] = m.inputs[i].Cursor.SetMode(m.cursorMode)
			}
			return m, tea.Batch(cmds...)
		}
	}

	// Update input fields with focus management
	cmd = m.updateInputs(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

func (m *Model) updateInputs(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, len(m.inputs))

	for i := range m.inputs {
		if i == m.focusIndex {
			m.inputs[i].PromptStyle = focusedStyle
			m.inputs[i].TextStyle = focusedStyle
			cmds[i] = m.inputs[i].Focus()
		} else {
			m.inputs[i].PromptStyle = noStyle
			m.inputs[i].TextStyle = noStyle
			m.inputs[i].Blur()
		}

		m.inputs[i], cmds[i] = m.inputs[i].Update(msg)
	}

	return tea.Batch(cmds...)
}

func (m *Model) View() string {
	var b strings.Builder

	// Render inputs
	for i := range m.inputs {
		b.WriteString(m.inputs[i].View())
		b.WriteRune('\n')
	}

	// Render submit button
	button := &blurredButton
	if m.focusIndex == len(m.inputs) {
		button = &focusedButton
	}
	fmt.Fprintf(&b, "\n%s\n", *button)

	// Cursor mode help
	b.WriteString(helpStyle.Render("cursor mode is "))
	b.WriteString(cursorModeHelpStyle.Render(m.cursorMode.String()))
	b.WriteString(helpStyle.Render(" (ctrl+r to change style)\n"))

	// Status and workers
	b.WriteString(fmt.Sprintf("\nStatus: %s\n", m.status))
	b.WriteString(fmt.Sprintf("Active Workers: %d/%d\n", m.activeWorkers, m.maxWorkers))

	// Search results
	b.WriteString("\nSearch Results:\n")
	for _, result := range m.searchResults {
		b.WriteString(result + "\n")
	}

	// Enhanced worker logs display
	b.WriteString("\nRecent Worker Activity:\n")
	for _, log := range m.workerLogs {
		b.WriteString(fmt.Sprintf(
			"Worker %d: %s (processed in %v)\n",
			log.WorkerID,
			log.URL,
			log.ProcessTime,
		))
	}

	// Workers statistics
	b.WriteString(fmt.Sprintf("\nActive Workers: %d/%d\n",
		atomic.LoadInt32(&m.activeWorkers), m.maxWorkers))

	return b.String()
}

func main() {
	dbPath := "crawler.db"
	model, err := initModel(dbPath)
	if err != nil {
		panic(err)
	}
	defer model.cancel()
	defer model.wg.Wait()
	defer model.db.Close()

	// Seed with multiple URLs to test concurrency
	initialURLs := []string{
		"https://en.wikipedia.org/wiki/List_of_programming_languages",
		"https://github.com/explore",
		"https://www.gutenberg.org/browse/scores/top",
		"https://news.ycombinator.com",
		"https://stackoverflow.com/questions",
		"https://www.reddit.com/r/programming/",
		"https://dev.to/",
		"https://www.infoq.com/",
		"https://hackernoon.com/",
		"https://medium.com/topic/programming",
	}

	for _, url := range initialURLs {
		model.urlQueue <- url
	}

	// Handle OS signals for graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		model.cancel()
	}()

	p := tea.NewProgram(model)
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running program: %v\n", err)
		os.Exit(1)
	}
}
