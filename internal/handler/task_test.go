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

func upload(t *testing.T, ts *httptest.Server, files map[string][]byte) (int, map[string]any) {
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

// start 调开始转换接口，返回 (code, 任务快照)。
func start(t *testing.T, ts *httptest.Server, id, concurrency, quality string) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("concurrency", concurrency); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("quality", quality); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.URL+"/api/tasks/"+id+"/start", mw.FormDataContentType(), &body)
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

// estimate 调预览接口，返回 (code, 响应)。
func estimate(t *testing.T, ts *httptest.Server, id, quality string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/tasks/" + id + "/estimate?quality=" + quality)
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

// waitForTask 轮询直至任务 done，超时 fatal。
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

func TestUploadCreatesReadyTask(t *testing.T) {
	dataDir := t.TempDir()
	outputs := t.TempDir()
	svc := service.New(store.New(), dataDir, outputs)
	ts := httptest.NewServer(handler.New(svc))
	t.Cleanup(ts.Close)

	code, resp := upload(t, ts, map[string][]byte{
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
	if resp["status"] != store.TaskReady {
		t.Errorf("status = %v, want ready (upload must not auto-start)", resp["status"])
	}
	dirName, _ := resp["dir_name"].(string)
	if dirName == "" {
		t.Error("response missing dir_name")
	}
	files, _ := resp["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	first, _ := files[0].(map[string]any)
	if first["status"] != store.StatusPending {
		t.Errorf("files[0].status = %v, want pending", first["status"])
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

func TestStartAndProcess(t *testing.T) {
	ts := newTestServer(t)

	_, resp := upload(t, ts, map[string][]byte{
		"a.jpg": tinyJPEG(t, 400, 300),
		"b.jpg": tinyJPEG(t, 400, 300),
	})
	id := resp["id"].(string)

	code, started := start(t, ts, id, "4", "80")
	if code != http.StatusOK {
		t.Fatalf("start status = %d, want 200; resp: %v", code, started)
	}
	if started["status"] != store.TaskProcessing {
		t.Errorf("status = %v, want processing", started["status"])
	}
	if started["concurrency"] != float64(4) {
		t.Errorf("concurrency = %v, want 4", started["concurrency"])
	}

	task := waitForTask(t, ts, id)
	if task["done"] != float64(2) {
		t.Errorf("done = %v, want 2", task["done"])
	}
}

func TestStartClampsParams(t *testing.T) {
	ts := newTestServer(t)

	for _, tc := range []struct {
		conc, qual  string
		wantConc    float64
		wantQuality float64
	}{
		{"0", "0", 1, 1},
		{"-5", "999", 1, 100},
		{"99", "abc", 32, 80},
		{"abc", "70", 1, 70},
	} {
		_, resp := upload(t, ts, map[string][]byte{"x.jpg": tinyJPEG(t, 100, 80)})
		id := resp["id"].(string)
		code, started := start(t, ts, id, tc.conc, tc.qual)
		if code != http.StatusOK {
			t.Fatalf("start status = %d, want 200", code)
		}
		if started["concurrency"] != tc.wantConc {
			t.Errorf("concurrency %q: got %v, want %v", tc.conc, started["concurrency"], tc.wantConc)
		}
		if started["quality"] != tc.wantQuality {
			t.Errorf("quality %q: got %v, want %v", tc.qual, started["quality"], tc.wantQuality)
		}
		waitForTask(t, ts, id)
	}
}

func TestStartUnknownTask(t *testing.T) {
	ts := newTestServer(t)
	code, _ := start(t, ts, "deadbeef", "1", "80")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestStartTwiceRejected(t *testing.T) {
	ts := newTestServer(t)

	_, resp := upload(t, ts, map[string][]byte{"a.jpg": tinyJPEG(t, 200, 150)})
	id := resp["id"].(string)

	if code, _ := start(t, ts, id, "1", "80"); code != http.StatusOK {
		t.Fatalf("first start = %d, want 200", code)
	}
	code, body := start(t, ts, id, "2", "90")
	if code != http.StatusConflict {
		t.Errorf("second start = %d, want 409", code)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Error("409 response missing error message")
	}
	waitForTask(t, ts, id)
}

func TestEstimate(t *testing.T) {
	ts := newTestServer(t)

	img := tinyJPEG(t, 800, 600)
	_, resp := upload(t, ts, map[string][]byte{
		"a.jpg":      img,
		"broken.txt": []byte("not an image"),
	})
	id := resp["id"].(string)

	code, est := estimate(t, ts, id, "50")
	if code != http.StatusOK {
		t.Fatalf("estimate status = %d, want 200", code)
	}
	if est["quality"] != float64(50) {
		t.Errorf("quality = %v, want 50", est["quality"])
	}
	files, _ := est["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("estimate files = %d, want 2", len(files))
	}
	byName := map[string]map[string]any{}
	for _, f := range files {
		fm := f.(map[string]any)
		byName[fm["orig_name"].(string)] = fm
	}
	a := byName["a.jpg"]
	if a["est"].(float64) <= 0 || a["est"].(float64) >= float64(len(img)) {
		t.Errorf("a.jpg est = %v, want 0 < est < %d", a["est"], len(img))
	}
	if byName["broken.txt"]["err"] == "" {
		t.Error("broken.txt missing error note")
	}
	if est["total"] != a["est"] {
		t.Errorf("total = %v, want %v (only successful files)", est["total"], a["est"])
	}

	// 质量越低预估越小
	_, est90 := estimate(t, ts, id, "90")
	files90 := est90["files"].([]any)
	var e50, e90 float64
	for _, f := range files {
		if f.(map[string]any)["orig_name"] == "a.jpg" {
			e50 = f.(map[string]any)["est"].(float64)
		}
	}
	for _, f := range files90 {
		if f.(map[string]any)["orig_name"] == "a.jpg" {
			e90 = f.(map[string]any)["est"].(float64)
		}
	}
	if e50 >= e90 {
		t.Errorf("q50 est %v should be < q90 est %v", e50, e90)
	}
}

func TestEstimateQualityClamped(t *testing.T) {
	ts := newTestServer(t)
	_, resp := upload(t, ts, map[string][]byte{"q.jpg": tinyJPEG(t, 100, 80)})
	id := resp["id"].(string)

	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"", 80},
		{"abc", 80},
		{"0", 1},
		{"100", 100},
		{"999", 100},
	} {
		code, est := estimate(t, ts, id, tc.in)
		if code != http.StatusOK {
			t.Fatalf("estimate status = %d, want 200", code)
		}
		if est["quality"] != tc.want {
			t.Errorf("quality %q: got %v, want %v", tc.in, est["quality"], tc.want)
		}
	}
}

func TestEstimateNonReady(t *testing.T) {
	ts := newTestServer(t)
	_, resp := upload(t, ts, map[string][]byte{"a.jpg": tinyJPEG(t, 200, 150)})
	id := resp["id"].(string)
	start(t, ts, id, "1", "80")
	waitForTask(t, ts, id)

	code, _ := estimate(t, ts, id, "80")
	if code != http.StatusConflict {
		t.Errorf("estimate after start = %d, want 409", code)
	}
}

func TestUploadNoFiles(t *testing.T) {
	ts := newTestServer(t)
	code, _ := upload(t, ts, nil)
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
}

func TestHistory(t *testing.T) {
	ts := newTestServer(t)
	_, hResp := upload(t, ts, map[string][]byte{"h.jpg": tinyJPEG(t, 100, 80)})
	start(t, ts, hResp["id"].(string), "1", "80")
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

	_, resp := upload(t, ts, map[string][]byte{
		"a.jpg":   tinyJPEG(t, 400, 300),
		"bad.txt": []byte("not an image"),
	})
	id := resp["id"].(string)
	dirName := resp["dir_name"].(string)
	start(t, ts, id, "2", "80")
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
	cd := dl.Header.Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "fileforge-"+dirName+".zip") {
		t.Errorf("Content-Disposition = %q, want attachment with fileforge-%s.zip", cd, dirName)
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

	_, resp := upload(t, ts, map[string][]byte{
		"a.jpg": tinyJPEG(t, 900, 700),
		"b.jpg": tinyJPEG(t, 900, 700),
		"c.jpg": tinyJPEG(t, 900, 700),
		"d.jpg": tinyJPEG(t, 900, 700),
	})
	id := resp["id"].(string)
	start(t, ts, id, "1", "80")

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

	_, resp := upload(t, ts, map[string][]byte{"a.jpg": tinyJPEG(t, 200, 150)})
	id := resp["id"].(string)
	start(t, ts, id, "1", "80")
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
