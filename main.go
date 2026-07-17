package main

import (
	"bufio"
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
	"github.com/manifoldco/promptui"
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
	Title    string `xml:"title"`
	Link     string `xml:"link"`
	InfoHash string `xml:"https://nyaa.si/xmlns/nyaa infoHash"`
	Size     string `xml:"https://nyaa.si/xmlns/nyaa size"`
	Seeders  string `xml:"https://nyaa.si/xmlns/nyaa seeders"`
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

// ------- Torrent Streaming via anacrolix/torrent -------

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true,
	".avi": true, ".webm": true, ".m4v": true,
}

func nyaaTorrentDownloadURL(viewLink string) string {
	viewLink = strings.TrimSuffix(viewLink, "/")
	parts := strings.Split(viewLink, "/")
	if id := parts[len(parts)-1]; id != "" {
		return fmt.Sprintf("%s/download/%s.torrent", nyaaBaseURL, id)
	}
	return ""
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

func buildMagnet(infoHash string) string {
	return fmt.Sprintf("magnet:?xt=urn:btih:%s", strings.ToLower(infoHash))
}

// Open the local video file using the OS default application.
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

	var t *torrent.Torrent
	torrentURL := nyaaTorrentDownloadURL(item.Link)
	if torrentURL != "" {
		torrentPath := filepath.Join(tmpDir, "torrent_data.torrent")
		fmt.Println("📥 Downloading torrent metadata file...")
		if err := downloadTorrentFile(torrentURL, torrentPath); err == nil {
			t, err = client.AddTorrentFromFile(torrentPath)
			if err != nil {
				log.Printf("⚠️ Could not load torrent file: %v", err)
				t = nil
			}
		}
	}

	if t == nil && item.InfoHash != "" {
		magnet := buildMagnet(item.InfoHash)
		fmt.Println("🔗 Fetching metadata via magnet link...")
		t, err = client.AddMagnet(magnet)
		if err != nil {
			return fmt.Errorf("add magnet: %w", err)
		}
	}

	if t == nil {
		return fmt.Errorf("no torrent metadata available")
	}

	fmt.Print("⏳ Resolving seeds and torrent pieces")
	select {
	case <-t.GotInfo():
		fmt.Println(" done!")
	case <-time.After(25 * time.Second):
		return fmt.Errorf("timed out waiting for torrent metadata. Seeders might be offline.")
	}

	// Pick the largest video file
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

	// OPTIMIZATION 1: First & Last Piece Prioritization
	// This lets players like mpv fetch the file index headers immediately without waiting.
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

	// OPTIMIZATION 2: Reduced buffering threshold
	// Decreased waiting threshold from 10MB to 1.5MB to initiate playback in under 3-5 seconds!
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

	// Open stream in the OS default player.
	openPlayer(streamURL)

	fmt.Println("\nPress [Enter] to terminate stream and clean up cache files...")
	var dummy string
	fmt.Scanln(&dummy)

	fmt.Println("🧹 Cleaning cached files...")
	return nil
}

// ------- Main CLI with Arrow-key Navigation -------

func main() {
	reader := bufio.NewReader(os.Stdin)

	// Cyberpunk-style Header
	fmt.Println("\x1b[38;5;45m" + `
  █████╗ ███╗   ██╗██╗ ██████╗██╗     ██╗
 ██╔══██╗████╗  ██║██║██╔════╝██║     ██║
 ███████║██╔██╗ ██║██║██║     ██║     ██║
 ██╔══██║██║╚██╗██║██║██║     ██║     ██║
 ██║  ██║██║ ╚████║██║╚██████╗███████╗██║
 ╚═╝  ╚═╝╚═╝  ╚═══╝╚═╝ ╚═════╝╚══════╝╚═╝` + "\x1b[0m")
	fmt.Println("\x1b[90m⚡ Nyaa.si Streamer & Fast Downloader | Version 2.0\x1b[0m\n")

	// No player detection is required; the OS default video player will be used.

	for {
		fmt.Print("\n🔍 Search Anime (or press Enter on 'exit' to quit): ")
		query, _ := reader.ReadString('\n')
		query = strings.TrimSpace(query)

		if query == "exit" || query == "quit" {
			break
		}
		if query == "" {
			continue
		}

		fmt.Printf("📡 Searching Nyaa.si for \"%s\"...\n", query)
		rssURL := nyaaRSSURL(query)
		rssData, err := fetchRSS(rssURL)
		if err != nil {
			fmt.Printf("❌ Failed to query Nyaa: %v\n", err)
			continue
		}

		items, err := parseItems(rssData)
		if err != nil {
			fmt.Printf("❌ Failed to parse XML output: %v\n", err)
			continue
		}

		if len(items) == 0 {
			fmt.Println("⚠️  No episodes match this query. Try adding keywords (e.g., '1080p' or sub group)!")
			continue
		}

		// List of options to navigate
		var options []string
		for _, item := range items {
			sizeInfo := ""
			if item.Size != "" {
				sizeInfo = fmt.Sprintf(" [%s]", item.Size)
			}
			options = append(options, fmt.Sprintf("%s%s", item.Title, sizeInfo))
		}
		options = append(options, "🔄 [Go Back / Search Again]")

		// Setup promptui Selector
		// Use plain templates on Windows to avoid readline ANSI handling bugs.
		templates := &promptui.SelectTemplates{
			Label:    "{{ . }}",
			Active:   "▶️ {{ . }}",
			Inactive: "   {{ . }}",
			Selected: "🍿 Selected: {{ . }}",
		}

		// On non-Windows platforms keep the colored templates for nicer UI
		if runtime.GOOS != "windows" {
			templates = &promptui.SelectTemplates{
				Label:    "{{ . }}",
				Active:   "▶️ \x1b[38;5;45m{{ . | cyan }}\x1b[0m",
				Inactive: "   {{ . }}",
				Selected: "🍿 Selected: \x1b[38;5;118m{{ . | green }}\x1b[0m",
			}
		}

		prompt := promptui.Select{
			Label:     "Select an episode using Arrow Keys (↑/↓) and press Enter:",
			Items:     options,
			Templates: templates,
			Size:      12,
		}

		idx, _, err := prompt.Run()
		if err != nil {
			fmt.Println("❌ Selection aborted.")
			continue
		}

		// Defensive: ensure idx is within bounds. The options slice includes
		// an extra "Go Back" entry, which is at index len(items).
		if idx < 0 || idx > len(items) {
			fmt.Println("❌ Invalid selection index, please try again.")
			continue
		}

		if idx == len(items) { // Go Back / Search Again
			continue
		}

		selected := &items[idx]
		fmt.Printf("\n🎬 Readying video stream for: %s\n", selected.Title)

		if err := streamTorrent(selected); err != nil {
			fmt.Printf("❌ Stream closed with error: %v\n", err)
		}
	}

	fmt.Println("\n\x1b[38;5;118mArigatou gozaimasu! Sayonara! 🌸\x1b[0m")
}
