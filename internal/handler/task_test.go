package handler_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fileforge/internal/handler"
	"fileforge/internal/service"
	"fileforge/internal/store"
)

func tinyJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), uint8((x + y) % 256), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	uploads := t.TempDir()
	outputs := t.TempDir()
	svc := service.New(store.New(), uploads, outputs)
	ts := httptest.NewServer(handler.New(svc))
	t.Cleanup(ts.Close)
	return ts
}

func upload(t *testing.T, ts *httptest.Server, concurrency, quality string, files map[string][]byte) (int, map[string]any) {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, content := range files {
		fw, err := mw.CreateFormFile("files[]", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(fw, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.WriteField("concurrency", concurrency); err != nil {
		t.Fatal(err)
	}
	if quality != "" {
		if err := mw.WriteField("quality", quality); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.URL+"/api/tasks", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response not JSON: %v; body: %s", err, raw)
	}
	return resp.StatusCode, out
}

// waitForTask 轮询直至任务 done（上传即异步开跑），超时 fatal。
func waitForTask(t *testing.T, ts *httptest.Server, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/api/tasks/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var task map[string]any
		err = json.NewDecoder(resp.Body).Decode(&task)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if task["status"] == store.TaskDone {
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not finish within deadline")
	return nil
}

func TestUploadCreatesTask(t *testing.T) {
	dataDir := t.TempDir()
	outputs := t.TempDir()
	svc := service.New(store.New(), dataDir, outputs)
	ts := httptest.NewServer(handler.New(svc))
	t.Cleanup(ts.Close)

	code, resp := upload(t, ts, "8", "80", map[string][]byte{
		"a.jpg": tinyJPEG(t, 320, 240),
		"b.jpg": tinyJPEG(t, 320, 240),
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; resp: %v", code, resp)
	}

	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatal("response missing non-empty id")
	}
	files, _ := resp["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	first, _ := files[0].(map[string]any)
	switch first["status"] {
	case store.StatusPending, store.StatusProcessing, store.StatusDone:
		// 上传即异步开跑，任一瞬态均可
	default:
		t.Errorf("files[0].status = %v, want a pending/processing/done", first["status"])
	}
	if first["orig_name"] != "a.jpg" {
		t.Errorf("files[0].orig_name = %v, want a.jpg", first["orig_name"])
	}
	if resp["quality"] != float64(80) {
		t.Errorf("quality = %v, want 80", resp["quality"])
	}

	// 文件已落盘 uploads/<id>/
	for _, name := range []string{"a.jpg", "b.jpg"} {
		p := filepath.Join(dataDir, id, name)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("uploaded file missing on disk: %v", err)
		}
		if len(data) == 0 {
			t.Errorf("uploaded file %s is empty", name)
		}
	}

	// GET /api/tasks/{id} 可查
	resp2, err := http.Get(ts.URL + "/api/tasks/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET task status = %d, want 200", resp2.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != id {
		t.Errorf("GET id = %v, want %v", got["id"], id)
	}
	if got["concurrency"] != float64(8) {
		t.Errorf("GET concurrency = %v, want 8", got["concurrency"])
	}
	if got["total"] != float64(2) {
		t.Errorf("GET total = %v, want 2", got["total"])
	}

	waitForTask(t, ts, id) // 与异步处理流同步，避免 TempDir 清理与 worker 竞争
}

func TestGetUnknownTask(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/tasks/deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestConcurrencyClamped(t *testing.T) {
	ts := newTestServer(t)

	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"0", 1},
		{"-5", 1},
		{"99", 32},
		{"abc", 1},
	} {
		_, resp := upload(t, ts, tc.in, "80", map[string][]byte{"x.jpg": tinyJPEG(t, 100, 80)})
		if resp["concurrency"] != tc.want {
			t.Errorf("concurrency %q: got %v, want %v", tc.in, resp["concurrency"], tc.want)
		}
		waitForTask(t, ts, resp["id"].(string))
	}
}

func TestQualityClamped(t *testing.T) {
	ts := newTestServer(t)

	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"", 80},
		{"abc", 80},
		{"1", 50},
		{"999", 95},
		{"70", 70},
	} {
		_, resp := upload(t, ts, "1", tc.in, map[string][]byte{"q.jpg": tinyJPEG(t, 100, 80)})
		if resp["quality"] != tc.want {
			t.Errorf("quality %q: got %v, want %v", tc.in, resp["quality"], tc.want)
		}
		waitForTask(t, ts, resp["id"].(string))
	}
}

func TestUploadNoFiles(t *testing.T) {
	ts := newTestServer(t)
	code, _ := upload(t, ts, "4", "80", nil)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestUploadDuplicateNames(t *testing.T) {
	dataDir := t.TempDir()
	outputs := t.TempDir()
	svc := service.New(store.New(), dataDir, outputs)
	ts := httptest.NewServer(handler.New(svc))
	t.Cleanup(ts.Close)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, content := range [][]byte{[]byte("first content"), []byte("second content")} {
		fw, err := mw.CreateFormFile("files[]", "same.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.WriteField("concurrency", "1"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.URL+"/api/tasks", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		ID    string `json:"id"`
		Files []struct {
			OrigName string `json:"orig_name"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(out.Files))
	}

	// 磁盘上两份内容都必须存在，不得互相覆盖
	entries, err := os.ReadDir(filepath.Join(dataDir, out.ID))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dataDir, out.ID, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		seen[string(data)] = true
	}
	if !seen["first content"] || !seen["second content"] {
		t.Errorf("expected both uploads preserved on disk, got %v", seen)
	}

	waitForTask(t, ts, out.ID)
}

func TestHistory(t *testing.T) {
	ts := newTestServer(t)
	_, hResp := upload(t, ts, "1", "80", map[string][]byte{"h.jpg": tinyJPEG(t, 100, 80)})
	waitForTask(t, ts, hResp["id"].(string))

	resp, err := http.Get(ts.URL + "/api/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var hist []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("len(history) = %d, want 1", len(hist))
	}
	if hist[0]["concurrency"] != float64(1) {
		t.Errorf("history[0].concurrency = %v, want 1", hist[0]["concurrency"])
	}
	if hist[0]["quality"] != float64(80) {
		t.Errorf("history[0].quality = %v, want 80", hist[0]["quality"])
	}
	if _, ok := hist[0]["elapsed_ms"]; !ok {
		t.Errorf("history[0] missing elapsed_ms: %v", hist[0])
	}
}

func TestDownloadZip(t *testing.T) {
	ts := newTestServer(t)

	_, resp := upload(t, ts, "2", "80", map[string][]byte{
		"a.jpg":   tinyJPEG(t, 400, 300),
		"bad.txt": []byte("not an image"),
	})
	id := resp["id"].(string)
	task := waitForTask(t, ts, id)
	if task["done"] != float64(2) {
		t.Fatalf("done = %v, want 2", task["done"])
	}

	dl, err := http.Get(ts.URL + "/api/tasks/" + id + "/download")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", dl.StatusCode)
	}
	if ct := dl.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}

	body, err := io.ReadAll(dl.Body)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a valid zip: %v", err)
	}
	if len(zr.File) != 1 {
		t.Fatalf("zip entries = %d, want 1 (only the done file, failed file excluded)", len(zr.File))
	}
	if zr.File[0].Name != "photo_001.jpg" {
		t.Errorf("zip entry name = %q, want photo_001.jpg", zr.File[0].Name)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("zip content is not a decodable image: %v", err)
	}
}

func TestDownloadUnknownTask(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/tasks/deadbeef/download")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// readEvents 读取 SSE 流直至 final 事件或超时，返回读到的帧。
func readEvents(t *testing.T, ts *httptest.Server, id string) []string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ts.URL + "/api/tasks/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	var frames []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			frames = append(frames, strings.TrimPrefix(line, "data: "))
			if strings.Contains(line, `"final":true`) {
				return frames
			}
		}
	}
	t.Fatalf("stream ended without final event; frames: %v", frames)
	return nil
}

func TestEventsStream(t *testing.T) {
	ts := newTestServer(t)

	_, resp := upload(t, ts, "1", "80", map[string][]byte{
		"a.jpg": tinyJPEG(t, 900, 700),
		"b.jpg": tinyJPEG(t, 900, 700),
		"c.jpg": tinyJPEG(t, 900, 700),
		"d.jpg": tinyJPEG(t, 900, 700),
	})
	id := resp["id"].(string)

	frames := readEvents(t, ts, id)
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}
	last := frames[len(frames)-1]
	if !strings.Contains(last, `"final":true`) {
		t.Errorf("last frame = %s, want final", last)
	}
	if !strings.Contains(last, `"done":4`) {
		t.Errorf("final frame = %s, want done=4", last)
	}
	waitForTask(t, ts, id)
}

func TestEventsAlreadyDone(t *testing.T) {
	ts := newTestServer(t)

	_, resp := upload(t, ts, "1", "80", map[string][]byte{"a.jpg": tinyJPEG(t, 200, 150)})
	id := resp["id"].(string)
	waitForTask(t, ts, id)

	frames := readEvents(t, ts, id)
	if len(frames) != 1 || !strings.Contains(frames[0], `"final":true`) {
		t.Errorf("frames = %v, want exactly one final frame (already-done task)", frames)
	}
}

func TestEventsUnknownTask(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/tasks/deadbeef/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
