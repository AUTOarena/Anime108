package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

// StreamVariant is one quality level advertised by the upstream master
// playlist.
type StreamVariant struct {
	Label      string `json:"label"`
	Resolution string `json:"resolution,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Bandwidth  int    `json:"bandwidth,omitempty"`
	URL        string `json:"-"`
}

// StreamInfo is everything needed to build a multi quality proxy session.
type StreamInfo struct {
	MasterURL string
	Referer   string
	Variants  []StreamVariant
}

// Best returns the highest quality variant, or an empty variant when none were
// found.
func (info StreamInfo) Best() StreamVariant {
	if len(info.Variants) == 0 {
		return StreamVariant{}
	}
	return info.Variants[0]
}

func (s *Scraper) fetchText(ctx context.Context, target, referer, origin string) (string, int, error) {
	resp, err := s.request(ctx, http.MethodGet, target, nil, map[string]string{"Referer": referer, "Origin": origin})
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	return string(data), resp.StatusCode, readErr
}

var (
	streamResolutionRE = regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)
	streamBandwidthRE  = regexp.MustCompile(`BANDWIDTH=(\d+)`)
	streamNameRE       = regexp.MustCompile(`NAME="([^"]+)"`)
)

// parseMasterPlaylist extracts every variant of a master playlist, sorted from
// the highest quality to the lowest. A media playlist (no #EXT-X-STREAM-INF)
// yields a single "auto" variant pointing at the playlist itself.
func parseMasterPlaylist(playlist, masterURL string) ([]StreamVariant, error) {
	base, err := url.Parse(masterURL)
	if err != nil {
		return nil, err
	}
	var variants []StreamVariant
	pending := StreamVariant{}
	hasPending := false
	for _, raw := range strings.Split(playlist, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			pending = StreamVariant{}
			hasPending = true
			if match := streamResolutionRE.FindStringSubmatch(line); len(match) > 2 {
				pending.Width, _ = strconv.Atoi(match[1])
				pending.Height, _ = strconv.Atoi(match[2])
				pending.Resolution = match[1] + "x" + match[2]
			}
			if match := streamBandwidthRE.FindStringSubmatch(line); len(match) > 1 {
				pending.Bandwidth, _ = strconv.Atoi(match[1])
			}
			if match := streamNameRE.FindStringSubmatch(line); len(match) > 1 {
				pending.Label = match[1]
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		reference, err := url.Parse(line)
		if err != nil {
			continue
		}
		variant := pending
		variant.URL = base.ResolveReference(reference).String()
		if variant.Label == "" {
			switch {
			case variant.Height > 0:
				variant.Label = strconv.Itoa(variant.Height) + "p"
			case variant.Bandwidth > 0:
				variant.Label = strconv.Itoa(variant.Bandwidth/1000) + "k"
			default:
				variant.Label = "auto"
			}
		}
		if !hasPending && len(variants) == 0 {
			// media playlist served directly, no quality levels advertised
			variant.Label = "auto"
		}
		variants = append(variants, variant)
		pending = StreamVariant{}
		hasPending = false
	}
	if len(variants) == 0 {
		return nil, errors.New("no streams found in master playlist")
	}
	sort.SliceStable(variants, func(i, j int) bool {
		if variants[i].Height != variants[j].Height {
			return variants[i].Height > variants[j].Height
		}
		return variants[i].Bandwidth > variants[j].Bandwidth
	})
	return variants, nil
}

// ResolveStreamVariants returns every quality level of an episode so the caller
// can expose a quality picker instead of hard coding the highest one.
func (s *Scraper) ResolveStreamVariants(ctx context.Context, iframeURL string) (StreamInfo, error) {
	info := StreamInfo{Referer: iframeURL}
	parsed, err := url.Parse(iframeURL)
	if err != nil {
		return info, err
	}
	videoID := parsed.Query().Get("id")
	if videoID == "" {
		return info, fmt.Errorf("no video ID found in iframe URL: %s", iframeURL)
	}
	playerDomain := parsed.Scheme + "://" + parsed.Host
	masterURL := fmt.Sprintf("%s/newplaylist/%s/%s.m3u8", playerDomain, videoID, videoID)
	playlist, status, err := s.fetchText(ctx, masterURL, iframeURL, playerDomain)
	if err != nil || status != http.StatusOK {
		masterURL = fmt.Sprintf("%s/newplaylist_g/%s/%s.m3u8", playerDomain, videoID, videoID)
		playlist, status, err = s.fetchText(ctx, masterURL, iframeURL, playerDomain)
	}
	if err != nil {
		return info, err
	}
	if status != http.StatusOK {
		return info, fmt.Errorf("master playlist returned HTTP %d", status)
	}
	variants, err := parseMasterPlaylist(playlist, masterURL)
	if err != nil {
		return info, err
	}
	info.MasterURL = masterURL

	// Some CDNs advertise "m3u8_g" variants that answer with an error body;
	// probe each level and fall back to its plain twin so that every quality
	// we expose is actually playable.
	usable := make([]StreamVariant, 0, len(variants))
	var lastStatus int
	for _, variant := range variants {
		content, code, fetchErr := s.fetchText(ctx, variant.URL, iframeURL, playerDomain)
		lastStatus = code
		if fetchErr == nil && code == http.StatusOK && !strings.Contains(content, "Error") {
			usable = append(usable, variant)
			continue
		}
		if strings.Contains(variant.URL, "m3u8_g") {
			alternate := strings.Replace(variant.URL, "m3u8_g", "m3u8", 1)
			content, code, fetchErr = s.fetchText(ctx, alternate, iframeURL, playerDomain)
			lastStatus = code
			if fetchErr == nil && code == http.StatusOK && !strings.Contains(content, "Error") {
				variant.URL = alternate
				usable = append(usable, variant)
			}
		}
	}
	if len(usable) == 0 {
		return info, fmt.Errorf("failed to fetch a valid stream playlist: HTTP %d", lastStatus)
	}
	info.Variants = usable
	return info, nil
}

// ResolveStreamURL keeps the single best quality behaviour for callers that do
// not need a quality picker.
func (s *Scraper) ResolveStreamURL(ctx context.Context, iframeURL string) (string, error) {
	info, err := s.ResolveStreamVariants(ctx, iframeURL)
	if err != nil {
		return "", err
	}
	return info.Best().URL, nil
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
