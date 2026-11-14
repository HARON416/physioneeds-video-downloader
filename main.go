package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const (
	referer                 = "https://www.e-physioneeds.com/"
	userAgent               = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
	playlistCaptureAttempts = 3
	playlistCaptureTimeout  = 15 * time.Second
	fetchMaxAttempts        = 4
	fetchInitialBackoff     = 250 * time.Millisecond
	maxConcurrentSegments   = 50
)

type videoDownload struct {
	masterURL  string
	outputFile string
}

type courseLesson struct {
	Section string `json:"section"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	ID      string `json:"id"`
}

type lessonCaptureReport struct {
	Downloads       []videoDownload
	CapturedLessons []courseLesson
	FailedLessons   []courseLesson
}

type playlistRequestCapture struct {
	mu  sync.RWMutex
	url string
}

type encryptionSpec struct {
	keyURL string
	iv     []byte
}

type segmentSpec struct {
	url        string
	keyURL     string
	iv         []byte
	sequenceNo uint64
}

type preparedVariant struct {
	segments []segmentSpec
	keys     map[string][]byte
}

type requestStats struct {
	active atomic.Int64
	peak   atomic.Int64
}

type apkIDCandidate struct {
	Source string `json:"source"`
	Value  string `json:"value"`
}

type apkIDProbeResult struct {
	Candidates []apkIDCandidate `json:"candidates"`
	Errors     []string         `json:"errors"`
}

func (stats *requestStats) start() {
	current := stats.active.Add(1)
	for peak := stats.peak.Load(); current > peak; peak = stats.peak.Load() {
		if stats.peak.CompareAndSwap(peak, current) {
			return
		}
	}
}

func (stats *requestStats) finish() {
	stats.active.Add(-1)
}

type preparedDownload struct {
	config        videoDownload
	videoManifest string
	audioManifest string
	video         preparedVariant
	audio         preparedVariant
}

var stdinReader = bufio.NewReader(os.Stdin)

func readInputLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	return strings.TrimSpace(line), err
}

func (capture *playlistRequestCapture) set(rawURL string) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.url = rawURL
}

func (capture *playlistRequestCapture) get() string {
	capture.mu.RLock()
	defer capture.mu.RUnlock()

	return capture.url
}

func (capture *playlistRequestCapture) clear() {
	capture.set("")
}

func (capture *playlistRequestCapture) wait(timeout time.Duration) string {
	if rawURL := capture.get(); rawURL != "" {
		return rawURL
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			if rawURL := capture.get(); rawURL != "" {
				return rawURL
			}
		case <-timer.C:
			return ""
		}
	}
}

func captureWithRetry(attempts int, capture func(attempt int) string) string {
	for attempt := 1; attempt <= attempts; attempt++ {
		if requestURL := capture(attempt); requestURL != "" {
			return requestURL
		}
	}
	return ""
}

func capitalizeTitle(value string) string {
	runes := []rune(strings.ToLower(strings.TrimSpace(value)))
	capitalizeNext := true
	for i, current := range runes {
		if unicode.IsLetter(current) {
			if capitalizeNext {
				runes[i] = unicode.ToUpper(current)
			}
			capitalizeNext = false
			continue
		}
		if unicode.IsDigit(current) {
			capitalizeNext = false
			continue
		}

		if unicode.IsSpace(current) || strings.ContainsRune("-/([{:", current) {
			capitalizeNext = true
		}
	}

	return string(runes)
}

func mp4OutputFile(title string) string {
	title = capitalizeTitle(title)
	if title == "" || strings.HasSuffix(strings.ToLower(title), ".mp4") {
		return title
	}

	return title + ".mp4"
}

func sanitizePathComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "uncategorized"
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSpace(r):
			builder.WriteRune(r)
		default:
			builder.WriteRune(' ')
		}
	}
	value = strings.Join(strings.Fields(builder.String()), " ")
	if value == "" {
		return "uncategorized"
	}
	return value
}

func courseOutputDir(courseTitle, section string) string {
	return filepath.Join("downloads", sanitizePathComponent(courseTitle), sanitizePathComponent(section))
}

var Logger = log.NewWithOptions(os.Stderr, log.Options{
	ReportTimestamp: true,
	TimeFormat:      time.Kitchen,
	Prefix:          "PHYSIONEEDS",
})

var successLogger = newStyledLogger(func(styles *log.Styles) {
	styles.Message = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styles.Levels[log.InfoLevel] = lipgloss.NewStyle().
		SetString(strings.ToUpper(log.InfoLevel.String())).
		Bold(true).
		MaxWidth(4).
		Foreground(lipgloss.Color("42"))
})

var promptLogger = newStyledLogger(func(styles *log.Styles) {
	styles.Levels[log.InfoLevel] = lipgloss.NewStyle().
		SetString(strings.ToUpper(log.InfoLevel.String())).
		Bold(true).
		MaxWidth(4)
})

var warningLogger = newStyledLogger(func(styles *log.Styles) {
	styles.Message = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styles.Levels[log.WarnLevel] = lipgloss.NewStyle().
		SetString(strings.ToUpper(log.WarnLevel.String())).
		Bold(true).
		MaxWidth(4).
		Foreground(lipgloss.Color("214"))
})

func newStyledLogger(customize func(*log.Styles)) *log.Logger {
	logger := log.NewWithOptions(os.Stderr, log.Options{
		ReportTimestamp: true,
		TimeFormat:      time.Kitchen,
		Prefix:          "PHYSIONEEDS",
	})

	styles := log.DefaultStyles()
	customize(styles)
	logger.SetStyles(styles)

	return logger
}

func logMessage(args ...any) string {
	return strings.ToUpper(strings.TrimSuffix(fmt.Sprintln(args...), "\n"))
}

func Info(args ...any) {
	Logger.Info(logMessage(args...))
}

func Infof(format string, args ...any) {
	Logger.Info(strings.ToUpper(fmt.Sprintf(format, args...)))
}

func Warn(args ...any) {
	warningLogger.Warn(logMessage(args...))
}

func Warnf(format string, args ...any) {
	warningLogger.Warn(strings.ToUpper(fmt.Sprintf(format, args...)))
}

func Successf(format string, args ...any) {
	successLogger.Info(strings.ToUpper(fmt.Sprintf(format, args...)))
}

func Prompt(args ...any) {
	promptLogger.Info(logMessage(args...))
}

func existingOutputFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect existing output %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("output path %q exists but is not a regular file", path)
	}
	return info.Size() > 0, nil
}

func videoLessonsInDOMOrder(items []courseLesson) []courseLesson {
	lessons := make([]courseLesson, 0, len(items))
	for _, item := range items {
		if !strings.EqualFold(strings.TrimSpace(item.Type), "video") || strings.TrimSpace(item.Title) == "" {
			continue
		}
		item.Section = strings.TrimSpace(item.Section)
		if item.Section == "" {
			item.Section = "Uncategorized"
		}
		lessons = append(lessons, item)
	}
	return lessons
}

func captureCourseLessons(
	lessons []courseLesson,
	courseTitle string,
	itemCount int,
	outputExists func(string) (bool, error),
	captureRequest func(courseLesson) string,
) (lessonCaptureReport, error) {
	report := lessonCaptureReport{
		Downloads:       make([]videoDownload, 0, len(lessons)),
		CapturedLessons: make([]courseLesson, 0, len(lessons)),
		FailedLessons:   make([]courseLesson, 0),
	}
	seenDownloads := make(map[videoDownload]struct{}, len(lessons))

	for i, lesson := range lessons {
		title := mp4OutputFile(lesson.Title)
		if lesson.ID == "" || title == "" {
			report.FailedLessons = append(report.FailedLessons, lesson)
			continue
		}

		outputFile := filepath.Join(courseOutputDir(courseTitle, lesson.Section), title)
		exists, err := outputExists(outputFile)
		if err != nil {
			return report, err
		}
		if exists {
			Infof("SKIPPING EXISTING LESSON FILE: %s", outputFile)
			continue
		}

		Infof("COURSE ITEM %d OF %d: %s -> %s", i+1, len(lessons), lesson.Title, outputFile)
		masterURL := captureRequest(lesson)
		if masterURL == "" {
			report.FailedLessons = append(report.FailedLessons, lesson)
			continue
		}

		download := videoDownload{masterURL: masterURL, outputFile: outputFile}
		if _, duplicate := seenDownloads[download]; duplicate {
			Warnf("SKIPPING DUPLICATE LESSON: %s", lesson.Title)
			continue
		}
		seenDownloads[download] = struct{}{}
		report.Downloads = append(report.Downloads, download)
		report.CapturedLessons = append(report.CapturedLessons, lesson)
		if itemCount > 0 && len(report.Downloads) >= itemCount {
			break
		}
	}

	return report, nil
}

func discoverCourseLessons(page *rod.Page) []courseLesson {
	page.MustEval(`() => {
		for (const section of document.querySelectorAll('.accordion.courseItem')) {
			if (section.querySelector('.courseSubItem')) {
				continue;
			}
			const header = section.querySelector('.title');
			if (header) {
				header.click();
			}
		}
	}`)

	payload := page.MustEval(`() => JSON.stringify(Array.from(document.querySelectorAll('.accordion.courseItem')).flatMap((section) => {
		const sectionTitle = (section.querySelector('.title') || null)?.textContent || section.dataset.title || 'Uncategorized';
		const items = Array.from(section.querySelectorAll('.courseSubItem'));
		return items.map((item) => {
			const title = (item.dataset.title || (item.querySelector('.title') || {}).textContent || '').trim();
			const type = (item.dataset.type || '').trim().toLowerCase();
			return { section: sectionTitle.trim() || 'Uncategorized', title, type, id: item.dataset.id || ''};
		});
	}))`).String()

	var items []courseLesson
	if err := json.Unmarshal([]byte(payload), &items); err != nil {
		Warnf("unable to decode lesson list: %v", err)
		return nil
	}
	return videoLessonsInDOMOrder(items)
}

func isPhysioneedsLoginPage(title, body, pageURL string) bool {
	title = strings.ToLower(strings.TrimSpace(title))
	body = strings.ToLower(strings.TrimSpace(body))
	pageURL = strings.ToLower(strings.TrimSpace(pageURL))

	if title == "login" || title == "log in" || strings.Contains(pageURL, "/t/public/login") {
		return true
	}
	return strings.Contains(body, "log in to physioneeds academy") &&
		strings.Contains(body, "continue with google") &&
		strings.Contains(body, "continue with email")
}

func waitForLoginIfNeeded(page *rod.Page) bool {
	if page == nil {
		return false
	}
	payload := page.MustEval(`() => {
		return JSON.stringify({
			title: document.title || '',
			body: document.body ? (document.body.innerText || '') : '',
			url: location.href || ''
		});
	}`).String()

	var info struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal([]byte(payload), &info); err != nil {
		Warnf("unable to decode login page detection payload: %v", err)
		return false
	}
	if !isPhysioneedsLoginPage(info.Title, info.Body, info.URL) {
		return false
	}

	Prompt("LOGIN REQUIRED: please log in to the Physioneeds Academy page in the browser and then press Enter here to continue.")
	if _, err := readInputLine(stdinReader); err != nil && !errors.Is(err, io.EOF) {
		panic(fmt.Errorf("read login confirmation: %w", err))
	} else if errors.Is(err, io.EOF) {
		panic("login confirmation input closed before login was confirmed")
	}
	Info("LOGIN CONFIRMATION RECEIVED; CHECKING THE PAGE AGAIN.")
	page.MustWaitLoad()
	return true
}

func getMasterURLsAndOutputFiles(itemCount int, startURL string, apkIDOnly, askForAPKID bool) (*rod.Browser, *rod.Page, lessonCaptureReport, string) {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	accountName := filepath.Base(cwd)
	profileDir := filepath.Join(cwd, "browser_profile")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		panic(err)
	}

	port, err := accountDebugPort(cwd)
	if err != nil {
		panic(err)
	}

	Infof("STARTING BROWSER - ACCOUNT: %s, PROFILE: %s, PORT: %d", accountName, profileDir, port)

	u := launcher.NewUserMode().
		Leakless(true).
		NoSandbox(true).
		Headless(false).
		Devtools(false).
		UserDataDir(profileDir).
		Set("remote-debugging-port", fmt.Sprintf("%d", port)).
		Set("disable-notifications").
		Devtools(true).
		MustLaunch()

	browser := rod.New().ControlURL(u).MustConnect().NoDefaultDevice()
	page := browser.MustPage()
	playlistCapture := &playlistRequestCapture{}
	router := page.HijackRequests()
	capturePlaylist := func(ctx *rod.Hijack) {
		requestURL := ctx.Request.URL()
		if strings.EqualFold(path.Base(requestURL.Path), "index.m3u8") {
			playlistCapture.set(requestURL.String())
		}
		ctx.ContinueRequest(&proto.FetchContinueRequest{})
	}
	for _, resourceType := range []proto.NetworkResourceType{
		proto.NetworkResourceTypeFetch,
		proto.NetworkResourceTypeXHR,
	} {
		if err := router.Add("*index.m3u8*", resourceType, capturePlaylist); err != nil {
			panic(fmt.Errorf("register %s playlist request capture: %w", resourceType, err))
		}
	}
	go router.Run()
	defer router.MustStop()

	page.MustNavigate(startURL).MustWindowMaximize().MustWaitLoad()
	for waitForLoginIfNeeded(page) {
		page.MustWaitLoad()
	}

	if apkIDOnly {
		return browser, page, lessonCaptureReport{}, discoverAndLogAPKID(page)
	}

	apkID := ""
	if askForAPKID {
		apkID = strings.TrimSpace(os.Getenv("SPAYEE_APK_ID"))
		if apkID == "" {
			probe := inspectPageAPKID(page)
			for _, warning := range probe.Errors {
				Warnf("APK-ID PAGE INSPECTION: %s", warning)
			}
			candidate, unique, distinctCount := uniqueAPKIDCandidate(probe.Candidates)
			switch {
			case unique:
				apkID = candidate.Value
				Infof("USING SPAYEE_APK_ID FROM THE LOGGED-IN PAGE (%s).", candidate.Source)
			case distinctCount > 1:
				Warnf("FOUND %d DISTINCT APK-ID VALUES; PLEASE PROVIDE SPAYEE_APK_ID.", distinctCount)
				apkID = promptForAPKID()
			default:
				Info("NO USABLE APK-ID FOUND IN THE LOGGED-IN PAGE.")
				apkID = promptForAPKID()
			}
		}
	}

	courseTitle := strings.TrimSpace(page.MustEval(`() => document.title || ''`).String())
	if courseTitle == "" {
		panic("course page title is empty; cannot create the course output folder")
	}

	lessons := discoverCourseLessons(page)
	if len(lessons) > 0 {
		Infof("DISCOVERED %d VIDEO LESSONS; STARTING FROM FIRST IN DOM ORDER: %s / %s", len(lessons), lessons[0].Section, lessons[0].Title)
	} else {
		Info("NO VIDEO LESSONS WERE DISCOVERED.")
	}

	captureRequest := func(lesson courseLesson) string {
		selector := fmt.Sprintf(`div.courseSubItem[data-id="%s"]`, lesson.ID)
		return captureWithRetry(playlistCaptureAttempts, func(attempt int) string {
			if attempt > 1 {
				Infof("RETRYING PLAYLIST CAPTURE FOR %s / %s (ATTEMPT %d OF %d)", lesson.Section, lesson.Title, attempt, playlistCaptureAttempts)
			}
			element, err := page.Element(selector)
			if err != nil {
				Warnf("unable to locate lesson %q in section %q: %v", lesson.Title, lesson.Section, err)
				return ""
			}
			isSelected := page.MustEval(`(lessonID) => {
				const item = Array.from(document.querySelectorAll('.courseSubItem')).find((candidate) => candidate.dataset.id === lessonID);
				return item ? item.classList.contains('selected') : false;
			}`, lesson.ID).Bool()
			if attempt > 1 || !isSelected {
				playlistCapture.clear()
			}
			element.MustClick()

			masterURL := playlistCapture.wait(playlistCaptureTimeout)
			if masterURL != "" {
				successLogger.Info("INDEX.M3U8 REQUEST URL", "section", lesson.Section, "title", lesson.Title, "url", masterURL)
			}
			return masterURL
		})
	}

	report, err := captureCourseLessons(lessons, courseTitle, itemCount, existingOutputFile, captureRequest)
	if err != nil {
		panic(err)
	}
	for _, lesson := range report.FailedLessons {
		Warnf("NO INDEX.M3U8 REQUEST WAS CAPTURED FOR %s / %s AFTER %d ATTEMPTS", lesson.Section, lesson.Title, playlistCaptureAttempts)
	}
	Infof("CAPTURE SUMMARY: %d OF %d VIDEO LESSONS CAPTURED; %d FAILED.", len(report.CapturedLessons), len(lessons), len(report.FailedLessons))

	downloadsPath := filepath.Join(cwd, "downloads.go")
	if err := saveDownloads(downloadsPath, report.Downloads); err != nil {
		panic(err)
	}
	Successf("SAVED %d DOWNLOADS TO %s", len(report.Downloads), downloadsPath)

	return browser, page, report, apkID
}

func discoverAndLogAPKID(page *rod.Page) string {
	result := inspectPageAPKID(page)
	for _, candidate := range result.Candidates {
		fmt.Printf("APK-ID candidate at %s: %s\n", candidate.Source, candidate.Value)
	}
	for _, err := range result.Errors {
		fmt.Printf("APK-ID inspection warning: %s\n", err)
	}
	candidate, unique, distinctCount := uniqueAPKIDCandidate(result.Candidates)
	if !unique {
		if distinctCount == 0 {
			fmt.Println("APK-ID discovery found no usable candidate values.")
		} else {
			fmt.Printf("APK-ID discovery found %d distinct usable values; none selected.\n", distinctCount)
		}
		return ""
	}
	fmt.Println("APK-ID discovery complete; exiting without using the value.")
	return candidate.Value
}

func inspectPageAPKID(page *rod.Page) apkIDProbeResult {
	raw := page.MustEval(`() => {
		const candidates = [];
		const errors = [];
		const matches = (name) => /apk[\W_]*id/i.test(name);
		const add = (source, value) => {
			if (typeof value !== 'string' && typeof value !== 'number') return;
			const text = String(value).trim();
			if (text) candidates.push({ source, value: text });
		};

		for (const key of Object.getOwnPropertyNames(window)) {
			if (!matches(key)) continue;
			try {
				add('window.' + key, window[key]);
			} catch (error) {
				errors.push('window.' + key + ': ' + String(error));
			}
		}

		for (const name of ['localStorage', 'sessionStorage']) {
			try {
				const storage = window[name];
				for (let i = 0; i < storage.length; i++) {
					const key = storage.key(i);
					if (key && matches(key)) add(name + '.' + key, storage.getItem(key));
				}
			} catch (error) {
				errors.push(name + ': ' + String(error));
			}
		}

		for (const [key, value] of new URLSearchParams(location.search)) {
			if (matches(key)) add('URL query.' + key, value);
		}
		for (const meta of document.querySelectorAll('meta[name], meta[property]')) {
			const key = meta.getAttribute('name') || meta.getAttribute('property') || '';
			if (matches(key)) add('meta.' + key, meta.content);
		}

		return JSON.stringify({ candidates, errors });
	}`).String()

	var result apkIDProbeResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		panic(fmt.Errorf("decode APK-ID page inspection: %w", err))
	}
	return result
}

func uniqueAPKIDCandidate(candidates []apkIDCandidate) (apkIDCandidate, bool, int) {
	byValue := make(map[string]apkIDCandidate)
	for _, candidate := range candidates {
		candidate.Value = strings.TrimSpace(candidate.Value)
		if candidate.Value == "" {
			continue
		}
		if _, err := hex.DecodeString(candidate.Value); err != nil {
			continue
		}
		key := strings.ToLower(candidate.Value)
		if _, exists := byValue[key]; !exists {
			byValue[key] = candidate
		}
	}
	if len(byValue) != 1 {
		return apkIDCandidate{}, false, len(byValue)
	}
	for _, candidate := range byValue {
		return candidate, true, 1
	}
	return apkIDCandidate{}, false, 0
}

func saveDownloads(filePath string, downloads []videoDownload) error {
	var output strings.Builder
	output.WriteString("package main\n\nvar downloads = []videoDownload{\n")
	for _, download := range downloads {
		output.WriteString("\t{\n")
		fmt.Fprintf(&output, "\t\tmasterURL:  %q,\n", download.masterURL)
		fmt.Fprintf(&output, "\t\toutputFile: %q,\n", download.outputFile)
		output.WriteString("\t},\n")
	}
	output.WriteString("}\n")

	if err := os.WriteFile(filePath, []byte(output.String()), 0o644); err != nil {
		return fmt.Errorf("write downloads file: %w", err)
	}

	return nil
}

func accountDebugPort(accountPath string) (int, error) {
	const (
		minPort = 9222
		maxPort = 65535
	)

	portRange := maxPort - minPort + 1
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(accountPath))
	start := minPort + int(hash.Sum32()%uint32(portRange))

	for i := range portRange {
		port := minPort + ((start - minPort + i) % portRange)
		if portAvailable(port) {
			return port, nil
		}
	}

	return 0, fmt.Errorf("no available browser debugging ports in range %d-%d", minPort, maxPort)
}

func portAvailable(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	return listener.Close() == nil
}

func promptForCourseURL() string {
	value := strings.TrimSpace(os.Getenv("PHYSIONEEDS_URL"))
	if value != "" {
		return value
	}
	for {
		fmt.Print("Course URL: ")
		line, err := readInputLine(stdinReader)
		if line != "" {
			return line
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				Warnf("unable to read course URL: %v", err)
			}
			return ""
		}
		fmt.Println("A course URL is required.")
	}
}

func promptForAPKID() string {
	value := strings.TrimSpace(os.Getenv("SPAYEE_APK_ID"))
	if value != "" {
		return value
	}
	fmt.Print("SPAYEE_APK_ID: ")
	value, err := readInputLine(stdinReader)
	if err != nil && value == "" {
		if !errors.Is(err, io.EOF) {
			Warnf("unable to read SPAYEE_APK_ID: %v", err)
		}
		return ""
	}
	return value
}

func countMP4Files(root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".mp4") {
			count++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("count MP4 files under %q: %w", root, err)
	}
	return count, nil
}

func validateDownload(download videoDownload) error {
	if download.masterURL == "" {
		return fmt.Errorf("masterURL is empty")
	}
	if download.outputFile == "" {
		return fmt.Errorf("outputFile is empty")
	}
	return nil
}

func parseMasterPlaylist(master []byte, masterURL string) (string, string, error) {
	baseURL := masterURL[:strings.LastIndex(masterURL, "/")+1]
	var audioManifest string
	var bestVideoManifest string
	var bestBandwidth int

	scanner := bufio.NewScanner(bytes.NewReader(master))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "#EXT-X-MEDIA") && strings.Contains(line, `TYPE=AUDIO`) {
			if uri := extractQuotedAttr(line, "URI"); uri != "" {
				audioManifest = resolveURL(baseURL, uri)
			}
		}

		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			if bwStr := extractUnquotedAttr(line, "BANDWIDTH"); bwStr != "" {
				if bw, _ := strconv.Atoi(bwStr); bw > bestBandwidth {
					bestBandwidth = bw
					bestVideoManifest = ""
				}
			}
		}

		if !strings.HasPrefix(line, "#") && line != "" && bestBandwidth > 0 && bestVideoManifest == "" {
			bestVideoManifest = resolveURL(baseURL, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("read master playlist: %w", err)
	}
	if bestVideoManifest == "" {
		return "", "", fmt.Errorf("no video manifest found")
	}
	return bestVideoManifest, audioManifest, nil
}

func prepareDownload(client *http.Client, download videoDownload, apkID string) (preparedDownload, error) {
	if err := validateDownload(download); err != nil {
		return preparedDownload{}, err
	}
	master, err := fetchBytes(client, download.masterURL)
	if err != nil {
		return preparedDownload{}, err
	}
	videoManifest, audioManifest, err := parseMasterPlaylist(master, download.masterURL)
	if err != nil {
		return preparedDownload{}, err
	}

	keyCache := make(map[string][]byte)
	video, err := prepareVariant(client, videoManifest, "video", apkID, keyCache)
	if err != nil {
		return preparedDownload{}, err
	}
	var audio preparedVariant
	if audioManifest != "" {
		audio, err = prepareVariant(client, audioManifest, "audio", apkID, keyCache)
		if err != nil {
			return preparedDownload{}, err
		}
	}
	return preparedDownload{
		config:        download,
		videoManifest: videoManifest,
		audioManifest: audioManifest,
		video:         video,
		audio:         audio,
	}, nil
}

func preflightDownloads(client *http.Client, items []videoDownload, apkID string) ([]preparedDownload, error) {
	prepared := make([]preparedDownload, len(items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	errorsCh := make(chan error, len(items))
	var completed atomic.Int64

	for i, item := range items {
		wg.Add(1)
		go func(idx int, download videoDownload) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result, err := prepareDownload(client, download, apkID)
			if err != nil {
				errorsCh <- fmt.Errorf("preflight %q: %w", download.outputFile, err)
				return
			}
			prepared[idx] = result
			done := completed.Add(1)
			fmt.Printf("[preflight] Prepared %d/%d: %s\n", done, len(items), download.outputFile)
		}(i, item)
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			return nil, err
		}
	}
	return prepared, nil
}

func downloadPreparedVideo(client *http.Client, download preparedDownload) error {
	if err := os.MkdirAll(filepath.Dir(download.config.outputFile), 0o755); err != nil {
		return err
	}

	videoTS, err := downloadVariant(client, download.video, "video", download.config.outputFile)
	if err != nil {
		return err
	}

	var audioTS string
	if download.audioManifest != "" {
		audioTS, err = downloadVariant(client, download.audio, "audio", download.config.outputFile)
		if err != nil {
			return err
		}
	}

	args := []string{"-y", "-i", videoTS}
	if audioTS != "" {
		args = append(args, "-i", audioTS)
	}
	args = append(args, "-c", "copy")
	if audioTS != "" {
		args = append(args, "-bsf:a", "aac_adtstoasc")
	}
	args = append(args, download.config.outputFile)
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}

	os.RemoveAll("video_segs")
	os.RemoveAll("audio_segs")
	os.Remove(videoTS)
	os.Remove(audioTS)

	fmt.Println("\nDone:", download.config.outputFile)
	return nil
}

func extractQuotedAttr(line, key string) string {
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `="([^"]+)"`)
	m := re.FindStringSubmatch(line)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

func extractUnquotedAttr(line, key string) string {
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `=([^,]+)`)
	m := re.FindStringSubmatch(line)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func resolveURL(base, ref string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return baseURL.ResolveReference(refURL).String()
}

func newRequest(rawURL string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

func fetchBytes(client *http.Client, rawURL string) ([]byte, error) {
	return fetchBytesWithStats(client, rawURL, nil)
}

func fetchBytesWithStats(client *http.Client, rawURL string, stats *requestStats) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= fetchMaxAttempts; attempt++ {
		req, err := newRequest(rawURL)
		if err != nil {
			return nil, err
		}
		if stats != nil {
			stats.start()
		}
		resp, err := client.Do(req)
		if err != nil {
			if stats != nil {
				stats.finish()
			}
			lastErr = fmt.Errorf("GET %s: %w", rawURL, err)
		} else {
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				if stats != nil {
					stats.finish()
				}
				lastErr = fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
				if resp.StatusCode != http.StatusTooManyRequests && (resp.StatusCode < 500 || resp.StatusCode >= 600) {
					return nil, lastErr
				}
			} else {
				data, readErr := io.ReadAll(resp.Body)
				closeErr := resp.Body.Close()
				if stats != nil {
					stats.finish()
				}
				if readErr != nil {
					lastErr = fmt.Errorf("read %s: %w", rawURL, readErr)
				} else if closeErr != nil {
					lastErr = fmt.Errorf("close response for %s: %w", rawURL, closeErr)
				} else {
					return data, nil
				}
			}
		}
		if attempt == fetchMaxAttempts {
			break
		}
		waitForFetchRetry(attempt)
	}
	return nil, lastErr
}

func waitForFetchRetry(attempt int) {
	time.Sleep(fetchInitialBackoff << (attempt - 1))
}

func parseIV(value string) ([]byte, error) {
	value = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(value), "0x"), "0X")
	if len(value) > aes.BlockSize*2 {
		return nil, fmt.Errorf("HLS IV is longer than %d bytes", aes.BlockSize)
	}
	if len(value)%2 != 0 {
		value = "0" + value
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("invalid HLS IV: %w", err)
	}
	iv := make([]byte, aes.BlockSize)
	copy(iv[len(iv)-len(decoded):], decoded)
	return iv, nil
}

func sequenceIV(sequence uint64) []byte {
	iv := make([]byte, aes.BlockSize)
	for i := len(iv) - 1; sequence > 0; i-- {
		iv[i] = byte(sequence)
		sequence >>= 8
	}
	return iv
}

func parseMediaPlaylist(data []byte, manifestURL string) ([]segmentSpec, error) {
	baseURL := manifestURL[:strings.LastIndex(manifestURL, "/")+1]
	var segments []segmentSpec
	var current *encryptionSpec
	var mediaSequence uint64

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			value := strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid media sequence %q: %w", value, err)
			}
			mediaSequence = parsed

		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			method := strings.Trim(extractUnquotedAttr(line, "METHOD"), `"`)
			if method == "NONE" {
				current = nil
				continue
			}
			if method != "AES-128" {
				return nil, fmt.Errorf("unsupported HLS encryption method %q", method)
			}
			keyRef := extractQuotedAttr(line, "URI")
			if keyRef == "" {
				return nil, fmt.Errorf("AES-128 key is missing its URI")
			}
			current = &encryptionSpec{keyURL: resolveURL(baseURL, keyRef)}
			if value := extractUnquotedAttr(line, "IV"); value != "" {
				iv, err := parseIV(value)
				if err != nil {
					return nil, err
				}
				current.iv = iv
			}

		case line != "" && !strings.HasPrefix(line, "#"):
			sequence := mediaSequence + uint64(len(segments))
			segment := segmentSpec{url: resolveURL(baseURL, line), sequenceNo: sequence}
			if current != nil {
				segment.keyURL = current.keyURL
				if current.iv != nil {
					segment.iv = append([]byte(nil), current.iv...)
				} else {
					segment.iv = sequenceIV(sequence)
				}
			}
			segments = append(segments, segment)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read media playlist: %w", err)
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("media playlist contains no segments")
	}
	return segments, nil
}

func apkKeyCandidates(apkID string) ([][]byte, error) {
	if apkID == "" {
		return nil, fmt.Errorf("set SPAYEE_APK_ID to the apkId from the course page")
	}

	apkID = strings.TrimSpace(apkID)
	if _, err := hex.DecodeString(apkID); err != nil {
		return nil, fmt.Errorf("SPAYEE_APK_ID must be hexadecimal: %w", err)
	}

	var encoded []string
	if len(apkID) >= 48 {
		encoded = append(encoded, apkID[16:48], apkID[:32])
		if len(apkID) >= 64 {
			encoded = append(encoded, apkID[len(apkID)-32:])
		}
		for start := 0; start+32 <= len(apkID); start++ {
			encoded = append(encoded, apkID[start:start+32])
		}
	} else {
		encoded = append(encoded, apkID)
	}

	var keys [][]byte
	seen := make(map[string]bool)
	for _, value := range encoded {
		key, err := hex.DecodeString(value)
		if err != nil {
			continue
		}
		if _, err := aes.NewCipher(key); err != nil {
			return nil, fmt.Errorf("invalid SPAYEE_APK_ID: %w", err)
		}
		if !seen[string(key)] {
			seen[string(key)] = true
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func unwrapMediaKeys(data []byte, apkID string) ([][]byte, error) {
	if len(data) == aes.BlockSize {
		return [][]byte{append([]byte(nil), data...)}, nil
	}
	if len(data) < 48 {
		return nil, fmt.Errorf("AES-128 key endpoint returned %d bytes; want 16 or at least 48", len(data))
	}
	apkKeys, err := apkKeyCandidates(apkID)
	if err != nil {
		return nil, fmt.Errorf("the HLS key is Graphy/Spayee-wrapped: %w", err)
	}
	layouts := []struct {
		intermediateOffset int
		mediaOffset        int
	}{
		{32, 8},
		{32, 0},
		{0, 32},
		{48, 16},
		{48, 0},
		{0, 48},
		{16, 48},
	}

	var candidates [][]byte
	seen := make(map[string]bool)
	for _, apkKey := range apkKeys {
		firstCipher, err := aes.NewCipher(apkKey)
		if err != nil {
			return nil, fmt.Errorf("create apkId cipher: %w", err)
		}
		for _, layout := range layouts {
			if layout.intermediateOffset+aes.BlockSize > len(data) || layout.mediaOffset+aes.BlockSize > len(data) {
				continue
			}
			intermediate := make([]byte, aes.BlockSize)
			firstCipher.Decrypt(intermediate, data[layout.intermediateOffset:layout.intermediateOffset+aes.BlockSize])
			secondCipher, err := aes.NewCipher(intermediate)
			if err != nil {
				return nil, fmt.Errorf("create media-key cipher: %w", err)
			}
			key := make([]byte, aes.BlockSize)
			secondCipher.Decrypt(key, data[layout.mediaOffset:layout.mediaOffset+aes.BlockSize])
			if !seen[string(key)] {
				seen[string(key)] = true
				candidates = append(candidates, key)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("wrapped key response contains no supported block layout")
	}
	return candidates, nil
}

func decryptSegment(data, key, iv []byte) ([]byte, error) {
	if len(key) != aes.BlockSize || len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("AES-128 requires a 16-byte key and IV")
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted segment size %d is not a positive multiple of %d", len(data), aes.BlockSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, data)

	padding := int(plain[len(plain)-1])
	if padding > 0 && padding <= aes.BlockSize && padding <= len(plain) {
		valid := true
		for _, b := range plain[len(plain)-padding:] {
			if int(b) != padding {
				valid = false
				break
			}
		}
		if valid {
			plain = plain[:len(plain)-padding]
		}
	}
	return plain, nil
}

func looksLikeMPEGTS(data []byte) bool {
	if len(data) < 188 || data[0] != 0x47 {
		return false
	}
	for offset, checked := 188, 1; offset < len(data) && checked < 4; offset, checked = offset+188, checked+1 {
		if data[offset] != 0x47 {
			return false
		}
	}
	return true
}

func selectMediaKey(client *http.Client, segment segmentSpec, candidates [][]byte) ([]byte, error) {
	encrypted, err := fetchBytes(client, segment.url)
	if err != nil {
		return nil, fmt.Errorf("fetch key-test segment: %w", err)
	}
	for _, key := range candidates {
		plain, err := decryptSegment(encrypted, key, segment.iv)
		if err == nil && looksLikeMPEGTS(plain) {
			return key, nil
		}
	}
	return nil, fmt.Errorf("none of the wrapped-key layouts decrypts as MPEG-TS; check that apkId and the playlist URL come from the same session")
}

func prepareVariant(client *http.Client, manifestURL, prefix, apkID string, keyCache map[string][]byte) (preparedVariant, error) {
	manifest, err := fetchBytes(client, manifestURL)
	if err != nil {
		return preparedVariant{}, fmt.Errorf("fetch %s manifest: %w", prefix, err)
	}
	segments, err := parseMediaPlaylist(manifest, manifestURL)
	if err != nil {
		return preparedVariant{}, fmt.Errorf("parse %s manifest: %w", prefix, err)
	}

	keys := make(map[string][]byte)
	for _, segment := range segments {
		if segment.keyURL == "" || keys[segment.keyURL] != nil {
			continue
		}
		if key := keyCache[segment.keyURL]; key != nil {
			keys[segment.keyURL] = key
			continue
		}
		wrapped, err := fetchBytes(client, segment.keyURL)
		if err != nil {
			return preparedVariant{}, fmt.Errorf("fetch %s encryption key: %w", prefix, err)
		}
		candidates, err := unwrapMediaKeys(wrapped, apkID)
		if err != nil {
			return preparedVariant{}, fmt.Errorf("prepare %s encryption key: %w", prefix, err)
		}
		key, err := selectMediaKey(client, segment, candidates)
		if err != nil {
			return preparedVariant{}, fmt.Errorf("select %s encryption key: %w", prefix, err)
		}
		keys[segment.keyURL] = key
		keyCache[segment.keyURL] = key
	}
	return preparedVariant{segments: segments, keys: keys}, nil
}

func segmentResumeID(outputFile, prefix string, variant preparedVariant) string {
	hash := fnv.New64a()
	_, _ = fmt.Fprintf(hash, "%s\n%s\n", outputFile, prefix)
	for _, segment := range variant.segments {
		for _, rawURL := range []string{segment.url, segment.keyURL} {
			parsed, err := url.Parse(rawURL)
			if err == nil {
				parsed.RawQuery = ""
				parsed.Fragment = ""
				rawURL = parsed.String()
			}
			_, _ = fmt.Fprintf(hash, "%s\n", rawURL)
		}
		_, _ = fmt.Fprintf(hash, "%x\n", segment.iv)
	}
	return fmt.Sprintf("%x", hash.Sum64())
}

func downloadVariant(client *http.Client, variant preparedVariant, prefix, outputFile string) (string, error) {
	total := len(variant.segments)
	fmt.Printf("[%s] Found %d segments (up to %d concurrent requests)\n", prefix, total, maxConcurrentSegments)
	resumeID := segmentResumeID(outputFile, prefix, variant)
	segDir := prefix + "_segs"
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return "", err
	}
	resumeKeyPath := filepath.Join(segDir, ".resume-key")
	previousResumeID, err := os.ReadFile(resumeKeyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	reuseExisting := err == nil && strings.TrimSpace(string(previousResumeID)) == resumeID
	if errors.Is(err, os.ErrNotExist) {
		reuseExisting = true
	} else if !reuseExisting {
		entries, err := os.ReadDir(segDir)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, "seg_") && (strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".tmp")) {
				if err := os.Remove(filepath.Join(segDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return "", err
				}
			}
		}
	}
	if err := os.WriteFile(resumeKeyPath, []byte(resumeID+"\n"), 0o644); err != nil {
		return "", err
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentSegments)
	errorsCh := make(chan error, total)
	paths := make([]string, total)
	var completed atomic.Int64
	stats := &requestStats{}
	reportProgress := func(done int64) {
		if done%10 == 0 || done == int64(total) {
			fmt.Printf("[%s] Downloaded %d/%d (active requests: %d, peak: %d)\n",
				prefix, done, total, stats.active.Load(), stats.peak.Load())
		}
	}

	for i, segment := range variant.segments {
		wg.Add(1)
		go func(idx int, spec segmentSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			path := filepath.Join(segDir, fmt.Sprintf("seg_%04d.ts", idx))
			if reuseExisting && validSegmentFile(path) {
				paths[idx] = path
				done := completed.Add(1)
				reportProgress(done)
				return
			}
			err := downloadSegment(client, spec, variant.keys[spec.keyURL], path, stats)
			if err != nil {
				errorsCh <- fmt.Errorf("%s segment %d: %w", prefix, idx, err)
				return
			}
			paths[idx] = path
			done := completed.Add(1)
			reportProgress(done)
		}(i, segment)
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			return "", err
		}
	}

	outName := prefix + ".ts"
	out, err := os.Create(outName)
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			out.Close()
			return "", err
		}
		_, copyErr := io.Copy(out, f)
		closeErr := f.Close()
		if copyErr != nil {
			out.Close()
			return "", copyErr
		}
		if closeErr != nil {
			out.Close()
			return "", closeErr
		}
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return outName, nil
}

func validSegmentFile(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.Size() < 188 {
		return false
	}
	size := info.Size()
	if size > 188*4 {
		size = 188 * 4
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(file, data); err != nil {
		return false
	}
	return looksLikeMPEGTS(data)
}

func downloadSegment(client *http.Client, segment segmentSpec, key []byte, path string, stats *requestStats) error {
	data, err := fetchBytesWithStats(client, segment.url, stats)
	if err != nil {
		return err
	}
	if segment.keyURL != "" {
		data, err = decryptSegment(data, key, segment.iv)
		if err != nil {
			return err
		}
		if !looksLikeMPEGTS(data) {
			return fmt.Errorf("decrypted data is not MPEG-TS; check that apkId and the playlist URL come from the same session")
		}
	}
	tempFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Chmod(0o644); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return nil
}

func main() {
	startURL := promptForCourseURL()
	if startURL == "" {
		Prompt("A course URL is required to continue.")
		return
	}

	captureOnly := strings.EqualFold(strings.TrimSpace(os.Getenv("PHYSIONEEDS_CAPTURE_ONLY")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("PHYSIONEEDS_CAPTURE_ONLY")), "true")
	apkIDOnly := strings.EqualFold(strings.TrimSpace(os.Getenv("PHYSIONEEDS_APK_ID_ONLY")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("PHYSIONEEDS_APK_ID_ONLY")), "true")
	itemCount := 0
	if captureOnly {
		itemCount = 1
	}

	askForAPKID := !captureOnly || apkIDOnly
	browser, _, report, apkID := getMasterURLsAndOutputFiles(itemCount, startURL, apkIDOnly, askForAPKID)
	if browser != nil {
		defer browser.MustClose()
	}
	if apkIDOnly {
		if apkID == "" {
			Warn("APK-ID DISCOVERY FAILED: no unique page value was found; no value was used.")
			return
		}
		Successf("APK-ID DISCOVERY COMPLETE; VALUE LOGGED AND NOT USED.")
		return
	}
	if captureOnly {
		if len(report.Downloads) == 0 {
			Prompt("CAPTURE-ONLY TEST FAILED: no pending lesson playlist request was captured.")
			return
		}
		Successf("CAPTURE-ONLY TEST PASSED: captured a playlist request for %s without downloading media.", report.Downloads[0].outputFile)
		return
	}
	if apkID == "" {
		Warn("SPAYEE_APK_ID is empty; provide the value from an authorized provider channel.")
	} else {
		Info("SPAYEE_APK_ID RECEIVED.")
	}
	if len(report.Downloads) == 0 {
		Prompt("No video downloads were queued. All lessons may already be downloaded, or the course URL/lesson list may be unavailable.")
		return
	}
	for i := range report.Downloads {
		if err := os.MkdirAll(filepath.Dir(report.Downloads[i].outputFile), 0o755); err != nil {
			panic(err)
		}
	}

	client := &http.Client{}
	prepared, err := preflightDownloads(client, report.Downloads, apkID)
	if err != nil {
		panic(err)
	}
	for i := range prepared {
		if err := downloadPreparedVideo(client, prepared[i]); err != nil {
			panic(fmt.Errorf("download %q: %w", prepared[i].config.outputFile, err))
		}
	}

	count, err := countMP4Files("downloads")
	if err != nil {
		panic(err)
	}
	Successf("Downloaded %d MP4 files into the categorized downloads directory.", count)
	if len(report.FailedLessons) > 0 {
		Warnf("CAPTURE INCOMPLETE: %d LESSONS HAD NO PLAYLIST REQUEST; REVIEW THE WARNINGS ABOVE.", len(report.FailedLessons))
	}
	Prompt("PROCESSING COMPLETE.")
}
