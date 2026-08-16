package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	baseURL   = "https://www.anime108.com"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

type Episode struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type Metadata struct {
	Title      string               `json:"title"`
	CleanTitle string               `json:"-"`
	PostID     int                  `json:"post_id"`
	Episode    int                  `json:"current_episode"`
	Server     int                  `json:"-"`
	Episodes   map[string][]Episode `json:"episodes"`
}

type SearchResult struct {
	Title        string `json:"title"`
	URL          string `json:"url"`
	Image        string `json:"image"`
	EpisodesInfo string `json:"episodes_info"`
}

type DownloadResult struct {
	Success  bool     `json:"success"`
	Filepath string   `json:"filepath,omitempty"`
	Metadata Metadata `json:"metadata"`
	StreamURL string   `json:"stream_url,omitempty"`
}

type ProgressFunc func(status string, current, total int, message string)

type Scraper struct {
	client *http.Client
}

func NewScraper() *Scraper {
	return &Scraper{client: &http.Client{Timeout: 30 * time.Second}}
}

func (s *Scraper) request(ctx context.Context, method, target string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return s.client.Do(req)
}

func responseBytes(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (s *Scraper) GetPageContent(ctx context.Context, target string) (string, error) {
	fmt.Printf("Fetching URL: %s\n", target)
	resp, err := s.request(ctx, http.MethodGet, target, nil, nil)
	if err != nil {
		return "", err
	}
	data, err := responseBytes(resp)
	return string(data), err
}

var (
	tagRE       = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRE     = regexp.MustCompile(`\s+`)
	unsafeName  = regexp.MustCompile(`[\\/*?:"<>|]`)
	halimCfgRE  = regexp.MustCompile(`(?s)var\s+halim_cfg\s*=\s*(\{.*?\});`)
	postIDRE    = regexp.MustCompile(`["']?post_id["']?\s*:\s*["']?(\d+)`)
	episodeRE   = regexp.MustCompile(`["']?episode["']?\s*:\s*["']?(\d+)`)
	serverRE    = regexp.MustCompile(`["']?server["']?\s*:\s*["']?(\d+)`)
	shortlinkRE = regexp.MustCompile(`(?is)<link[^>]+rel=["']shortlink["'][^>]+href=["'][^"']*\?p=(\d+)[^"']*["']|<link[^>]+href=["'][^"']*\?p=(\d+)[^"']*["'][^>]+rel=["']shortlink["']`)
	h1RE        = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	titleRE     = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

func plainText(value string) string {
	return strings.TrimSpace(spaceRE.ReplaceAllString(html.UnescapeString(tagRE.ReplaceAllString(value, " ")), " "))
}

func firstInt(re *regexp.Regexp, value string, fallback int) int {
	match := re.FindStringSubmatch(value)
	if len(match) > 1 {
		if number, err := strconv.Atoi(match[1]); err == nil {
			return number
		}
	}
	return fallback
}

func absoluteAnimeURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return value
	}
	base, _ := url.Parse(baseURL)
	return base.ResolveReference(parsed).String()
}

func attribute(tag, name string) string {
	re := regexp.MustCompile(`(?is)\b` + regexp.QuoteMeta(name) + `\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	match := re.FindStringSubmatch(tag)
	for i := 1; i < len(match); i++ {
		if match[i] != "" {
			return html.UnescapeString(match[i])
		}
	}
	return ""
}

func parseSelect(document, id string) []Episode {
	selectRE := regexp.MustCompile(`(?is)<select\b[^>]*\bid\s*=\s*["']` + regexp.QuoteMeta(id) + `["'][^>]*>(.*?)</select>`)
	selectMatch := selectRE.FindStringSubmatch(document)
	if len(selectMatch) < 2 {
		return []Episode{}
	}
	optionRE := regexp.MustCompile(`(?is)<option\b([^>]*)>(.*?)</option>`)
	options := optionRE.FindAllStringSubmatch(selectMatch[1], -1)
	result := make([]Episode, 0, len(options))
	for _, option := range options {
		target := attribute(option[1], "value")
		if target != "" {
			result = append(result, Episode{Title: plainText(option[2]), URL: absoluteAnimeURL(target)})
		}
	}
	return result
}

func (s *Scraper) ParseShowPage(document, sourceURL string) (Metadata, error) {
	metadata := Metadata{Episode: 1, Server: 1, Episodes: map[string][]Episode{"Thai": {}, "Sound Track": {}}}
	if cfg := halimCfgRE.FindStringSubmatch(document); len(cfg) > 1 {
		metadata.PostID = firstInt(postIDRE, cfg[1], 0)
		metadata.Episode = firstInt(episodeRE, cfg[1], 1)
		metadata.Server = firstInt(serverRE, cfg[1], 1)
	}
	if metadata.PostID == 0 {
		if match := shortlinkRE.FindStringSubmatch(document); len(match) > 1 {
			for _, value := range match[1:] {
				if value != "" {
					metadata.PostID, _ = strconv.Atoi(value)
					break
				}
			}
		}
	}
	if match := h1RE.FindStringSubmatch(document); len(match) > 1 {
		metadata.Title = plainText(match[1])
	} else if match := titleRE.FindStringSubmatch(document); len(match) > 1 {
		metadata.Title = plainText(match[1])
	} else {
		metadata.Title = "Anime Video"
	}
	metadata.CleanTitle = strings.TrimSpace(unsafeName.ReplaceAllString(metadata.Title, ""))
	metadata.Episodes["Thai"] = parseSelect(document, "sequel_select_th")
	metadata.Episodes["Sound Track"] = parseSelect(document, "sequel_select_en")
	return metadata, nil
}

func (s *Scraper) GetPlayerIframe(ctx context.Context, postID, episode, server int, lang, title string) (string, error) {
	form := url.Values{
		"action": {"halim_ajax_player"}, "nonce": {""}, "episode": {strconv.Itoa(episode)},
		"server": {strconv.Itoa(server)}, "postid": {strconv.Itoa(postID)}, "lang": {lang}, "title": {title},
	}
	resp, err := s.request(ctx, http.MethodPost, baseURL+"/api/get.php", strings.NewReader(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Origin": baseURL, "Referer": baseURL + "/", "X-Requested-With": "XMLHttpRequest",
	})
	if err != nil {
		return "", err
	}
	data, err := responseBytes(resp)
	if err != nil {
		return "", err
	}
	body := strings.ReplaceAll(string(data), `\`, "")
	iframeRE := regexp.MustCompile(`(?is)\bsrc\s*=\s*["']([^"']+)["']`)
	match := iframeRE.FindStringSubmatch(body)
	if len(match) < 2 {
		return "", fmt.Errorf("player iframe not found in API response")
	}
	if strings.HasPrefix(match[1], "//") {
		return "https:" + match[1], nil
	}
	return match[1], nil
}

type streamVariant struct{ resolution, path string }

func (s *Scraper) fetchText(ctx context.Context, target, referer, origin string) (string, int, error) {
	resp, err := s.request(ctx, http.MethodGet, target, nil, map[string]string{"Referer": referer, "Origin": origin})
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	return string(data), resp.StatusCode, readErr
}

func (s *Scraper) ResolveStreamURL(ctx context.Context, iframeURL string) (string, error) {
	parsed, err := url.Parse(iframeURL)
	if err != nil {
		return "", err
	}
	videoID := parsed.Query().Get("id")
	if videoID == "" {
		return "", fmt.Errorf("no video ID found in iframe URL: %s", iframeURL)
	}
	playerDomain := parsed.Scheme + "://" + parsed.Host
	masterURL := fmt.Sprintf("%s/newplaylist/%s/%s.m3u8", playerDomain, videoID, videoID)
	playlist, status, err := s.fetchText(ctx, masterURL, iframeURL, playerDomain)
	if err != nil || status != http.StatusOK {
		masterURL = fmt.Sprintf("%s/newplaylist_g/%s/%s.m3u8", playerDomain, videoID, videoID)
		playlist, status, err = s.fetchText(ctx, masterURL, iframeURL, playerDomain)
	}
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("master playlist returned HTTP %d", status)
	}
	resolutionRE := regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)
	var variants []streamVariant
	current := "Unknown"
	for _, raw := range strings.Split(playlist, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			if match := resolutionRE.FindStringSubmatch(line); len(match) > 2 {
				current = match[1] + "x" + match[2]
			}
		} else if line != "" && !strings.HasPrefix(line, "#") {
			variants = append(variants, streamVariant{current, line})
			current = "Unknown"
		}
	}
	if len(variants) == 0 {
		return "", errors.New("no streams found in master playlist")
	}
	sort.SliceStable(variants, func(i, j int) bool {
		width := func(value string) int { n, _ := strconv.Atoi(strings.Split(value, "x")[0]); return n }
		return width(variants[i].resolution) > width(variants[j].resolution)
	})
	candidate, err := url.Parse(variants[0].path)
	if err != nil {
		return "", err
	}
	masterBase, err := url.Parse(masterURL)
	if err != nil {
		return "", err
	}
	streamURL := masterBase.ResolveReference(candidate).String()
	content, status, err := s.fetchText(ctx, streamURL, iframeURL, playerDomain)
	if err == nil && status == http.StatusOK && !strings.Contains(content, "Error") {
		return streamURL, nil
	}
	if strings.Contains(streamURL, "m3u8_g") {
		alternate := strings.Replace(streamURL, "m3u8_g", "m3u8", 1)
		content, status, err = s.fetchText(ctx, alternate, iframeURL, playerDomain)
		if err == nil && status == http.StatusOK && !strings.Contains(content, "Error") {
			return alternate, nil
		}
	}
	return "", fmt.Errorf("failed to fetch a valid stream playlist: HTTP %d", status)
}

func (s *Scraper) GetSegments(ctx context.Context, streamURL, iframeURL string) ([]string, error) {
	content, status, err := s.fetchText(ctx, streamURL, iframeURL, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("stream playlist returned HTTP %d", status)
	}
	base, _ := url.Parse(streamURL)
	segments := []string{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		reference, err := url.Parse(line)
		if err == nil {
			segments = append(segments, base.ResolveReference(reference).String())
		}
	}
	return segments, nil
}

func (s *Scraper) downloadSegment(ctx context.Context, target string, index int, tempDir, iframeURL string, retries int) (string, error) {
	path := filepath.Join(tempDir, fmt.Sprintf("%05d.aaa", index))
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return path, nil
	}
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		resp, err := s.request(ctx, http.MethodGet, target, nil, map[string]string{"Referer": iframeURL})
		if err != nil {
			lastErr = err
			continue
		}
		data, err := responseBytes(resp)
		if err == nil {
			err = os.WriteFile(path, data, 0o644)
		}
		if err == nil {
			return path, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func mergeSegments(files []string, outputPath string) error {
	tempPath := outputPath + ".temp.ts"
	output, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(output)
	for _, path := range files {
		input, openErr := os.Open(path)
		if openErr != nil {
			output.Close()
			return openErr
		}
		_, copyErr := io.Copy(writer, input)
		input.Close()
		if copyErr != nil {
			output.Close()
			return copyErr
		}
	}
	if err = writer.Flush(); err != nil {
		output.Close()
		return err
	}
	if err = output.Close(); err != nil {
		return err
	}
	if _, err = exec.LookPath("ffmpeg"); err == nil {
		cmd := exec.Command("ffmpeg", "-y", "-i", tempPath, "-c", "copy", outputPath)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if runErr := cmd.Run(); runErr == nil {
			return os.Remove(tempPath)
		} else {
			fmt.Printf("FFmpeg failed, preserving concatenated stream: %s\n", stderr.String())
		}
	}
	_ = os.Remove(outputPath)
	return os.Rename(tempPath, outputPath)
}

func (s *Scraper) DownloadVideo(ctx context.Context, episodeURL, outputDir, lang string, concurrency int, checkOnly bool, progress ProgressFunc) (DownloadResult, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return DownloadResult{}, err
	}
	document, err := s.GetPageContent(ctx, episodeURL)
	if err != nil {
		return DownloadResult{}, err
	}
	metadata, err := s.ParseShowPage(document, episodeURL)
	if err != nil {
		return DownloadResult{}, err
	}
	if metadata.PostID == 0 {
		return DownloadResult{}, errors.New("could not find post ID on the page")
	}
	iframeURL, err := s.GetPlayerIframe(ctx, metadata.PostID, metadata.Episode, metadata.Server, lang, metadata.Title)
	if err != nil {
		return DownloadResult{}, err
	}
	streamURL, err := s.ResolveStreamURL(ctx, iframeURL)
	if err != nil {
		return DownloadResult{}, err
	}
	if checkOnly {
		return DownloadResult{Success: true, Metadata: metadata, StreamURL: streamURL}, nil
	}
	segments, err := s.GetSegments(ctx, streamURL, iframeURL)
	if err != nil {
		return DownloadResult{}, err
	}
	if len(segments) == 0 {
		return DownloadResult{}, errors.New("no segments found in playlist")
	}
	tempDir := filepath.Join(outputDir, fmt.Sprintf("temp_%d_ep%d_%s", metadata.PostID, metadata.Episode, strings.ReplaceAll(lang, " ", "")))
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return DownloadResult{}, err
	}
	files := make([]string, len(segments))
	jobs := make(chan int)
	var completed atomic.Int64
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				path, downloadErr := s.downloadSegment(ctx, segments[index], index, tempDir, iframeURL, 3)
				if downloadErr == nil {
					files[index] = path
				}
				current := int(completed.Add(1))
				if progress != nil {
					progress("downloading", current, len(segments), fmt.Sprintf("Downloading chunk %d/%d", current, len(segments)))
				}
			}
		}()
	}
	for index := range segments {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	for index, path := range files {
		if path == "" {
			files[index], err = s.downloadSegment(ctx, segments[index], index, tempDir, iframeURL, 5)
			if err != nil {
				return DownloadResult{}, fmt.Errorf("segment %d could not be downloaded: %w", index, err)
			}
		}
	}
	outputName := fmt.Sprintf("%s - Ep %d (%s).mp4", metadata.CleanTitle, metadata.Episode, lang)
	outputPath := filepath.Join(outputDir, outputName)
	if progress != nil {
		progress("merging", len(files), len(files), "Merging segments into output MP4 file...")
	}
	if err := mergeSegments(files, outputPath); err != nil {
		return DownloadResult{}, err
	}
	for _, path := range files {
		_ = os.Remove(path)
	}
	_ = os.Remove(tempDir)
	if progress != nil {
		progress("completed", len(files), len(files), "Saved final MP4 to: "+outputPath)
	}
	return DownloadResult{Success: true, Filepath: outputPath, Metadata: metadata}, nil
}

func balancedDivBlocks(document, className string) []string {
	tokenRE := regexp.MustCompile(`(?is)</?div\b[^>]*>`)
	tokens := tokenRE.FindAllStringIndex(document, -1)
	var blocks []string
	for i, token := range tokens {
		tag := document[token[0]:token[1]]
		if strings.HasPrefix(strings.ToLower(tag), "</") || !hasClass(attribute(tag, "class"), className) {
			continue
		}
		depth := 1
		for j := i + 1; j < len(tokens); j++ {
			candidate := strings.ToLower(document[tokens[j][0]:tokens[j][1]])
			if strings.HasPrefix(candidate, "</div") {
				depth--
			} else {
				depth++
			}
			if depth == 0 {
				blocks = append(blocks, document[token[0]:tokens[j][1]])
				break
			}
		}
	}
	return blocks
}

func hasClass(classes, wanted string) bool {
	for _, className := range strings.Fields(classes) {
		if className == wanted {
			return true
		}
	}
	return false
}

func firstTag(block, tag string) (string, string) {
	re := regexp.MustCompile(`(?is)<` + regexp.QuoteMeta(tag) + `\b([^>]*)>(.*?)</` + regexp.QuoteMeta(tag) + `>|<` + regexp.QuoteMeta(tag) + `\b([^>]*)/?>`)
	match := re.FindStringSubmatch(block)
	if len(match) == 0 {
		return "", ""
	}
	attrs := match[1]
	if attrs == "" && len(match) > 3 {
		attrs = match[3]
	}
	return attrs, match[2]
}

func contentByClass(block, tag, className string) string {
	re := regexp.MustCompile(`(?is)<` + tag + `\b([^>]*)>(.*?)</` + tag + `>`)
	for _, match := range re.FindAllStringSubmatch(block, -1) {
		if hasClass(attribute(match[1], "class"), className) {
			return plainText(match[2])
		}
	}
	return ""
}

func (s *Scraper) SearchAnime(ctx context.Context, keyword string) ([]SearchResult, error) {
	target := baseURL + "/search_movie?" + url.Values{"keyword": {keyword}}.Encode()
	resp, err := s.request(ctx, http.MethodGet, target, nil, map[string]string{"Referer": baseURL + "/"})
	if err != nil {
		return nil, err
	}
	data, err := responseBytes(resp)
	if err != nil {
		return nil, err
	}
	results := []SearchResult{}
	for _, block := range balancedDivBlocks(string(data), "box") {
		anchorAttrs, _ := firstTag(block, "a")
		href := attribute(anchorAttrs, "href")
		if href == "" {
			continue
		}
		imageAttrs, _ := firstTag(block, "img")
		image := attribute(imageAttrs, "data-lazy-src")
		if image == "" || strings.HasPrefix(image, "data:image") {
			image = attribute(imageAttrs, "src")
		}
		title := contentByClass(block, "div", "p2")
		if title == "" {
			title = attribute(imageAttrs, "alt")
		}
		episodeInfo := contentByClass(block, "span", "EP")
		if episodeInfo == "" {
			episodeInfo = contentByClass(block, "span", "update")
		}
		results = append(results, SearchResult{Title: title, URL: absoluteAnimeURL(href), Image: image, EpisodesInfo: episodeInfo})
	}
	return results, nil
}
