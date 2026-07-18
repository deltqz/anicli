package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ------- Styles & UI -------

var (
	appStyle   = lipgloss.NewStyle().Padding(1, 2)
	titleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("45")).Bold(true)
	subStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	banner = `
  █████╗ ███╗   ██╗██╗ ██████╗██╗     ██╗
 ██╔══██╗████╗  ██║██║██╔════╝██║     ██║
 ███████║██╔██╗ ██║██║██║     ██║     ██║
 ██╔══██║██║╚██╗██║██║██║     ██║     ██║
 ██║  ██║██║ ╚████║██║╚██████╗███████╗██║
 ╚═╝  ╚═╝╚═╝  ╚═══╝╚═╝ ╚═════╝╚══════╝╚═╝`
)

// ------- RSS Parsing -------

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title   string `xml:"title"`
	Link    string `xml:"link"`
	Size    string `xml:"https://nyaa.si/xmlns/nyaa size"`
	Seeders string `xml:"https://nyaa.si/xmlns/nyaa seeders"`
}

const nyaaBaseURL = "https://nyaa.si"

func nyaaRSSURL(query string) string {
	if query == "" {
		return fmt.Sprintf("%s/?page=rss", nyaaBaseURL)
	}
	return fmt.Sprintf("%s/?page=rss&q=%s", nyaaBaseURL, url.QueryEscape(query))
}

func fetchRSS(url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	req.Header.Set("User-Agent", "Anicli/2.0 (torrent streaming tool)")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("rss feed returned status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(body), nil
}

func parseItems(xmlData string) ([]rssItem, error) {
	var feed rssFeed
	if err := xml.Unmarshal([]byte(xmlData), &feed); err != nil {
		return nil, fmt.Errorf("xml: %w", err)
	}
	return feed.Channel.Items, nil
}

// ------- Bubble Tea TUI Model -------

type sessionState int

const (
	stateSearch sessionState = iota
	stateFetching
	stateChoosing
)

type listItem struct {
	rssItem
}

// list.Item interface implementation
func (i listItem) Title() string { return i.rssItem.Title }
func (i listItem) Description() string {
	return fmt.Sprintf("📦 Size: %s | 🌱 Seeders: %s", i.rssItem.Size, i.rssItem.Seeders)
}
func (i listItem) FilterValue() string { return i.rssItem.Title }

type rssResultMsg []rssItem
type errMsg struct{ err error }

func (e errMsg) Error() string { return e.err.Error() }

type model struct {
	state     sessionState
	textInput textinput.Model
	spinner   spinner.Model
	list      list.Model
	selected  *rssItem
	err       error
	quitting  bool
}

func initialModel() model {
	ti := textinput.New()
	ti.Placeholder = "e.g. 'Jujutsu Kaisen 1080p'"
	ti.Focus()
	ti.CharLimit = 156
	ti.Width = 50

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	l := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Select Episode"
	l.SetShowStatusBar(true)
	l.Styles.Title = lipgloss.NewStyle().Background(lipgloss.Color("45")).Foreground(lipgloss.Color("0")).Padding(0, 1)

	return model{
		state:     stateSearch,
		textInput: ti,
		spinner:   sp,
		list:      l,
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "esc":
			if m.state == stateChoosing {
				m.state = stateSearch
				m.list.ResetSelected()
				return m, nil
			}
			m.quitting = true
			return m, tea.Quit
		case "enter":
			if m.state == stateSearch && m.textInput.Value() != "" {
				m.state = stateFetching
				m.err = nil
				return m, tea.Batch(m.spinner.Tick, fetchTorrentsCmd(m.textInput.Value()))
			}
			if m.state == stateChoosing {
				if i, ok := m.list.SelectedItem().(listItem); ok {
					m.selected = &i.rssItem
					return m, tea.Quit
				}
			}
		}

	case tea.WindowSizeMsg:
		h, v := appStyle.GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v)
		m.textInput.Width = msg.Width - h - 4

	case rssResultMsg:
		items := make([]list.Item, len(msg))
		for i, res := range msg {
			items[i] = listItem{rssItem: res}
		}
		m.list.SetItems(items)
		m.state = stateChoosing
		return m, nil

	case errMsg:
		m.err = msg.err
		m.state = stateSearch
		return m, nil
	}

	switch m.state {
	case stateSearch:
		m.textInput, cmd = m.textInput.Update(msg)
		cmds = append(cmds, cmd)
	case stateFetching:
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
	case stateChoosing:
		m.list, cmd = m.list.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	if m.quitting && m.selected == nil {
		return ""
	}

	header := titleStyle.Render(banner) + "\n" +
		subStyle.Render("⚡Watch Nyaa.si torrents from your terminal") + "\n\n"

	var content string
	switch m.state {
	case stateSearch:
		content = "🔍 Search Anime:\n\n" + m.textInput.View()
		if m.err != nil {
			content += "\n\n" + errorStyle.Render(fmt.Sprintf("❌ Error: %v", m.err))
		}
		content += "\n\n(ESC to quit, ENTER to search)"
	case stateFetching:
		content = fmt.Sprintf("%s Searching Nyaa.si for '%s'...", m.spinner.View(), m.textInput.Value())
	case stateChoosing:
		return appStyle.Render(m.list.View())
	}

	return appStyle.Render(header + content)
}

func fetchTorrentsCmd(query string) tea.Cmd {
	return func() tea.Msg {
		rssURL := nyaaRSSURL(query)
		data, err := fetchRSS(rssURL)
		if err != nil {
			return errMsg{fmt.Errorf("failed to fetch: %w", err)}
		}
		items, err := parseItems(data)
		if err != nil {
			return errMsg{fmt.Errorf("failed to parse: %w", err)}
		}
		if len(items) == 0 {
			return errMsg{fmt.Errorf("no results found")}
		}
		return rssResultMsg(items)
	}
}

// ------- Torrent Streaming via anacrolix/torrent -------

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true,
	".avi": true, ".webm": true, ".m4v": true,
}

func downloadTorrentFile(url, destPath string) error {
	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	req.Header.Set("User-Agent", "Anicli/2.0 (torrent streaming tool)")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func openPlayer(streamURL string) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", streamURL)
	case "darwin":
		cmd = exec.Command("open", streamURL)
	default:
		cmd = exec.Command("xdg-open", streamURL)
	}

	if cmd != nil {
		if err := cmd.Start(); err != nil {
			log.Printf("❌ Failed to start default player: %v", err)
			fmt.Printf("Please open this stream URL manually in your player:\n%s\n", streamURL)
		}
	}
}

func streamTorrent(item *rssItem) error {
	tmpDir, err := os.MkdirTemp("", "anicli-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = tmpDir
	cfg.Seed = false

	client, err := torrent.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("torrent client: %w", err)
	}
	defer client.Close()

	// The Link extracted from the RSS item is ALREADY the exact download URL
	torrentURL := item.Link
	if torrentURL == "" {
		return fmt.Errorf("could not determine the .torrent download URL from link")
	}

	torrentPath := filepath.Join(tmpDir, "torrent_data.torrent")
	fmt.Printf("📥 Downloading torrent file from: %s...\n", torrentURL)

	if err := downloadTorrentFile(torrentURL, torrentPath); err != nil {
		return fmt.Errorf("failed to download .torrent file: %w", err)
	}

	t, err := client.AddTorrentFromFile(torrentPath)
	if err != nil {
		return fmt.Errorf("failed to load local .torrent file: %w", err)
	}

	fmt.Print("⏳ Reading torrent metadata")
	select {
	case <-t.GotInfo():
		fmt.Println(" done!")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timed out processing local .torrent file")
	}

	// Pick largest video file
	var targetFile *torrent.File
	for _, f := range t.Files() {
		ext := strings.ToLower(filepath.Ext(f.Path()))
		if videoExts[ext] {
			if targetFile == nil || f.Length() > targetFile.Length() {
				targetFile = f
			}
		}
	}

	if targetFile == nil {
		return fmt.Errorf("no playable video file found in the torrent")
	}

	// High priority for our target file
	targetFile.SetPriority(torrent.PiecePriorityHigh)

	firstPiece := targetFile.BeginPieceIndex()
	lastPiece := targetFile.EndPieceIndex() - 1

	if lastPiece >= firstPiece {
		t.Piece(firstPiece).SetPriority(torrent.PiecePriorityNow)
		if firstPiece+1 < lastPiece {
			t.Piece(firstPiece + 1).SetPriority(torrent.PiecePriorityNow)
		}
		t.Piece(lastPiece).SetPriority(torrent.PiecePriorityNow)
	}

	t.DownloadAll()
	t.DisallowDataUpload()

	fmt.Print("🚀 Fast-buffering essential playback indexes")
	deadline := time.Now().Add(45 * time.Second)
	buffered := false
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		fmt.Print(".")

		if targetFile.BytesCompleted() > 1500000 { // ~1.5 MB
			fmt.Println(" ready!")
			buffered = true
			break
		}
	}

	if !buffered {
		fmt.Println(" speed is slow, launching stream server anyway (may experience initial buffering)...")
	}

	completed := targetFile.BytesCompleted()
	total := targetFile.Length()
	fmt.Printf("\n🎬 Playing: %s\n📦 Buffered: %.1f MB / Total: %.1f MB\n",
		targetFile.DisplayPath(),
		float64(completed)/1024/1024,
		float64(total)/1024/1024)

	// Stream server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("socket listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reader := targetFile.NewReader()
		defer reader.Close()
		http.ServeContent(w, r, targetFile.DisplayPath(), time.Now(), reader)
	})

	httpServer := &http.Server{Handler: mux}
	go httpServer.Serve(listener)
	defer httpServer.Close()

	videoName := url.PathEscape(targetFile.DisplayPath())
	streamURL := fmt.Sprintf("http://127.0.0.1:%d/%s", port, videoName)
	fmt.Printf("🌍 Streaming Server: %s\n", streamURL)

	openPlayer(streamURL)

	fmt.Println("\nPress [Enter] to terminate stream and clean up cache files...")
	var dummy string
	fmt.Scanln(&dummy)

	fmt.Println("🧹 Cleaning cached files...")
	return nil
}

// ------- Application Root Loop -------

func main() {
	for {
		m := initialModel()

		// Run TUI in Alternate Screen to not clutter terminal history during search
		p := tea.NewProgram(m, tea.WithAltScreen())

		fm, err := p.Run()
		if err != nil {
			fmt.Printf("Error starting CLI interface: %v\n", err)
			os.Exit(1)
		}

		finalModel, ok := fm.(model)
		if !ok || (finalModel.quitting && finalModel.selected == nil) {
			break
		}

		if finalModel.selected != nil {
			// Back in normal terminal screen:
			fmt.Printf("\n🎬 Readying video stream for: \x1b[38;5;45m%s\x1b[0m\n\n", finalModel.selected.Title)

			if err := streamTorrent(finalModel.selected); err != nil {
				fmt.Printf("❌ Stream closed with error: %v\n", err)
				time.Sleep(2 * time.Second)
			}
		}
	}
}
