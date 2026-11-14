package main

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCapitalizeTitle(t *testing.T) {
	t.Parallel()

	input := "  MUSCLE TEAR AND STRAIN - RIGID TAPE  "
	want := "Muscle Tear And Strain - Rigid Tape"
	if got := capitalizeTitle(input); got != want {
		t.Fatalf("capitalizeTitle(%q) = %q, want %q", input, got, want)
	}
}

func TestMP4OutputFile(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"KYPHOSIS 1":     "Kyphosis 1.mp4",
		"KYPHOSIS 1.MP4": "Kyphosis 1.mp4",
		"   ":            "",
	}
	for input, want := range tests {
		if got := mp4OutputFile(input); got != want {
			t.Errorf("mp4OutputFile(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCourseOutputDir(t *testing.T) {
	t.Parallel()

	got := courseOutputDir("Certificate Course", "Live Classes")
	want := filepath.Join("downloads", "Certificate Course", "Live Classes")
	if got != want {
		t.Fatalf("courseOutputDir() = %q, want %q", got, want)
	}
}

func TestCourseLessonCaptureCoversModulesAndReportsMissingRequest(t *testing.T) {
	t.Parallel()

	lessons := videoLessonsInDOMOrder([]courseLesson{
		{Section: "Live Classes", Title: "Live webinar", Type: "video", ID: "live-1"},
		{Section: "Foundation", Title: "Introduction PDF", Type: "pdf", ID: "foundation-pdf"},
		{Section: "Foundation", Title: "Introduction", Type: "VIDEO", ID: "foundation-intro"},
		{Section: "Assessment and Diagnosis", Title: "Muscle Structure", Type: "video", ID: "assessment-1"},
		{Section: "Palpation Upper Quadrant", Title: "Levator Scapulae", Type: "video", ID: "palpation-1"},
	})

	wantIDs := []string{"live-1", "foundation-intro", "assessment-1", "palpation-1"}
	if len(lessons) != len(wantIDs) {
		t.Fatalf("videoLessonsInDOMOrder() returned %d lessons, want %d", len(lessons), len(wantIDs))
	}
	for i, wantID := range wantIDs {
		if lessons[i].ID != wantID {
			t.Errorf("lesson %d ID = %q, want %q", i, lessons[i].ID, wantID)
		}
	}

	var attempted []string
	report, err := captureCourseLessons(
		lessons,
		"Course",
		0,
		func(string) (bool, error) { return false, nil },
		func(lesson courseLesson) string {
			attempted = append(attempted, lesson.ID)
			if lesson.ID == "foundation-intro" {
				return ""
			}
			return "https://media.example/" + lesson.ID + "/index.m3u8"
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(attempted) != len(lessons) {
		t.Fatalf("capture attempted %d lessons, want all %d", len(attempted), len(lessons))
	}
	if len(report.Downloads) != 3 || len(report.CapturedLessons) != 3 {
		t.Fatalf("captured %d downloads / %d lessons, want 3 / 3", len(report.Downloads), len(report.CapturedLessons))
	}
	if len(report.FailedLessons) != 1 {
		t.Fatalf("reported %d failed lessons, want 1", len(report.FailedLessons))
	}
	if got := report.FailedLessons[0]; got.ID != "foundation-intro" || got.Section != "Foundation" {
		t.Fatalf("failed lesson = %#v, want the Foundation Introduction lesson", got)
	}
	if got, want := report.Downloads[2].outputFile, filepath.Join("downloads", "Course", "Palpation Upper Quadrant", "Levator Scapulae.mp4"); got != want {
		t.Errorf("last captured output = %q, want %q", got, want)
	}
}

func TestCourseLessonCaptureReportsExistingAndDuplicateOutputs(t *testing.T) {
	t.Parallel()

	lessons := []courseLesson{
		{Section: "Module", Title: "Same Lesson", Type: "video", ID: "lesson-1"},
		{Section: "Module", Title: "Same Lesson", Type: "video", ID: "lesson-2"},
	}
	var checkedPaths []string
	report, err := captureCourseLessons(
		lessons,
		"Course",
		0,
		func(path string) (bool, error) {
			checkedPaths = append(checkedPaths, path)
			return true, nil
		},
		func(courseLesson) string { t.Fatal("existing outputs should not trigger playlist capture"); return "" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SkippedExisting) != 2 || report.UniqueOutputPaths != 2 || report.NumberedDuplicateEntries != 1 {
		t.Fatalf("existing/unique/numbered-duplicate counts = %d/%d/%d, want 2/2/1",
			len(report.SkippedExisting), report.UniqueOutputPaths, report.NumberedDuplicateEntries)
	}
	wantPaths := []string{
		filepath.Join("downloads", "Course", "Module", "Same Lesson 1.mp4"),
		filepath.Join("downloads", "Course", "Module", "Same Lesson 2.mp4"),
	}
	if len(checkedPaths) != len(wantPaths) {
		t.Fatalf("checked %d output paths, want %d", len(checkedPaths), len(wantPaths))
	}
	for i, want := range wantPaths {
		if checkedPaths[i] != want {
			t.Errorf("output path %d = %q, want %q", i, checkedPaths[i], want)
		}
	}
}

func TestAllocateDuplicateOutputPathsAvoidsNaturalSuffixCollision(t *testing.T) {
	t.Parallel()

	basePath := filepath.Join("downloads", "Course", "Module", "Same Lesson.mp4")
	naturallyNumberedPath := filepath.Join("downloads", "Course", "Module", "Same Lesson 1.mp4")
	got, duplicateEntries := allocateLessonOutputPaths([]string{
		basePath,
		basePath,
		naturallyNumberedPath,
	})
	want := []string{
		filepath.Join("downloads", "Course", "Module", "Same Lesson 2.mp4"),
		filepath.Join("downloads", "Course", "Module", "Same Lesson 3.mp4"),
		naturallyNumberedPath,
	}
	if duplicateEntries != 1 {
		t.Fatalf("duplicate entries = %d, want 1", duplicateEntries)
	}
	seen := make(map[string]struct{}, len(got))
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("output path %d = %q, want %q", i, got[i], want[i])
		}
		if _, exists := seen[got[i]]; exists {
			t.Errorf("output path %q was assigned more than once", got[i])
		}
		seen[got[i]] = struct{}{}
	}
}

func TestExistingOutputFile(t *testing.T) {
	t.Parallel()

	outputFile := filepath.Join(t.TempDir(), "lesson.mp4")
	exists, err := existingOutputFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("missing output file was reported as existing")
	}

	if err := os.WriteFile(outputFile, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	exists, err = existingOutputFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("non-empty output file was not reported as existing")
	}
}

func TestReadInputLine(t *testing.T) {
	t.Parallel()

	line, err := readInputLine(bufio.NewReader(strings.NewReader("  continue  \n")))
	if err != nil {
		t.Fatalf("readInputLine() error = %v", err)
	}
	if line != "continue" {
		t.Fatalf("readInputLine() = %q, want %q", line, "continue")
	}
}

func TestCaptureWithRetry(t *testing.T) {
	t.Parallel()

	t.Run("eventually captures request", func(t *testing.T) {
		attempts := 0
		got := captureWithRetry(3, func(attempt int) string {
			attempts++
			if attempt == 2 {
				return "https://media.example/lesson/index.m3u8"
			}
			return ""
		})
		if got != "https://media.example/lesson/index.m3u8" || attempts != 2 {
			t.Fatalf("captureWithRetry() = %q after %d attempts, want request after 2 attempts", got, attempts)
		}
	})

	t.Run("stops after configured attempts", func(t *testing.T) {
		attempts := 0
		got := captureWithRetry(3, func(int) string {
			attempts++
			return ""
		})
		if got != "" || attempts != 3 {
			t.Fatalf("captureWithRetry() = %q after %d attempts, want empty after 3 attempts", got, attempts)
		}
	})
}

func TestFetchBytesRetriesServerErrors(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("segment"))
	}))
	defer server.Close()

	data, err := fetchBytes(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "segment" {
		t.Fatalf("fetchBytes() = %q, want %q", data, "segment")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
}

func TestFetchBytesTracksActiveRequests(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	stats := &requestStats{}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := fetchBytesWithStats(server.Client(), server.URL, stats)
			errs <- err
		}()
	}

	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("both requests did not reach the server")
		}
	}
	if got := stats.active.Load(); got != 2 {
		close(release)
		t.Fatalf("active request count = %d, want 2", got)
	}
	if got := stats.peak.Load(); got != 2 {
		close(release)
		t.Fatalf("peak request count = %d, want 2", got)
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := stats.active.Load(); got != 0 {
		t.Fatalf("active request count after completion = %d, want 0", got)
	}
}

func TestUniqueAPKIDCandidate(t *testing.T) {
	t.Parallel()

	const apkID = "3334663633334663636363137643230643064613563631376432306430646135"
	got, unique, count := uniqueAPKIDCandidate([]apkIDCandidate{
		{Source: "window.apkId", Value: apkID},
		{Source: "localStorage.apkId", Value: strings.ToUpper(apkID)},
		{Source: "window.invalidApkId", Value: "not-hex"},
	})
	if !unique || count != 1 || got.Source != "window.apkId" || got.Value != apkID {
		t.Fatalf("uniqueAPKIDCandidate() = %#v, %v, %d; want one valid ID from window.apkId", got, unique, count)
	}

	_, unique, count = uniqueAPKIDCandidate([]apkIDCandidate{
		{Source: "window.apkId", Value: apkID},
		{Source: "localStorage.apkId", Value: "abcd"},
	})
	if unique || count != 2 {
		t.Fatalf("uniqueAPKIDCandidate() with distinct values = unique %v, count %d; want false, 2", unique, count)
	}
}

func TestDownloadVariantReusesValidSegments(t *testing.T) {
	t.Parallel()

	segmentData := make([]byte, 188*4)
	for i := 0; i < len(segmentData); i += 188 {
		segmentData[i] = 0x47
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(segmentData)
	}))
	defer server.Close()

	prefix := filepath.Join(t.TempDir(), "video")
	segmentDir := prefix + "_segs"
	if err := os.MkdirAll(segmentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingPath := filepath.Join(segmentDir, "seg_0000.ts")
	if err := os.WriteFile(existingPath, segmentData, 0o644); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(segmentDir, "seg_0001.ts")
	if err := os.WriteFile(partialPath, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	variant := preparedVariant{segments: []segmentSpec{
		{url: server.URL + "/segment-0"},
		{url: server.URL + "/segment-1"},
	}}
	output, err := downloadVariant(server.Client(), variant, prefix, "lesson.mp4")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), segmentData...), segmentData...)
	if !bytes.Equal(got, want) {
		t.Fatal("joined output does not contain both complete segments in playlist order")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestDownloadVariantDoesNotReuseSegmentsFromAnotherLesson(t *testing.T) {
	t.Parallel()

	oldData := make([]byte, 188*4)
	newData := make([]byte, 188*4)
	for i := 0; i < len(oldData); i += 188 {
		oldData[i] = 0x47
		newData[i] = 0x47
	}
	newData[1] = 0x01
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(newData)
	}))
	defer server.Close()

	prefix := filepath.Join(t.TempDir(), "video")
	segmentDir := prefix + "_segs"
	if err := os.MkdirAll(segmentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(segmentDir, "seg_0000.ts"), oldData, 0o644); err != nil {
		t.Fatal(err)
	}
	oldVariant := preparedVariant{segments: []segmentSpec{{url: server.URL + "/old"}}}
	oldResumeID := segmentResumeID("old-lesson.mp4", "video", oldVariant)
	if err := os.WriteFile(filepath.Join(segmentDir, ".resume-key"), []byte(oldResumeID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	variant := preparedVariant{segments: []segmentSpec{{url: server.URL + "/new"}}}
	output, err := downloadVariant(server.Client(), variant, prefix, "new-lesson.mp4")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newData) {
		t.Fatal("joined output reused a segment from a different lesson")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestSaveDownloadsAsGo(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), "downloads.go")
	err := saveDownloads(filePath, []videoDownload{{
		masterURL:  "https://example.com/index.m3u8",
		outputFile: "Kyphosis 1.mp4",
	}})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	for _, want := range []string{
		"var downloads = []videoDownload{",
		"masterURL:  \"https://example.com/index.m3u8\"",
		"outputFile: \"Kyphosis 1.mp4\"",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("generated file does not contain %q:\n%s", want, output)
		}
	}
}

func TestIsPhysioneedsLoginPage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		title string
		body  string
		url   string
		want  bool
	}{
		{
			name:  "login title",
			title: "Log In",
			want:  true,
		},
		{
			name:  "modal body matches login page",
			title: "Physioneeds Academy",
			body:  "Log in to Physioneeds Academy Continue with Google Continue with email",
			want:  true,
		},
		{
			name:  "login redirect URL",
			title: "Physioneeds",
			url:   "https://www.e-physioneeds.com/t/public/login",
			want:  true,
		},
		{
			name:  "course page is not login screen",
			title: "Certificate in Orthopaedic Physiotherapy",
			body:  "Introduction to anatomy Sign up Login",
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPhysioneedsLoginPage(tc.title, tc.body, tc.url); got != tc.want {
				t.Fatalf("isPhysioneedsLoginPage(%q, %q, %q) = %v, want %v", tc.title, tc.body, tc.url, got, tc.want)
			}
		})
	}
}
