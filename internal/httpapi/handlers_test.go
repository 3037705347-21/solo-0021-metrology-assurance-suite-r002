package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"example.com/solo-0021-metrology-assurance-suite/internal/bootstrap"
)

type client struct {
	t      *testing.T
	server *httptest.Server
}

func newClient(t *testing.T) *client {
	t.Helper()
	server := httptest.NewServer(bootstrap.NewHandler())
	t.Cleanup(server.Close)
	return &client{t: t, server: server}
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequest(method, c.server.URL+path, reader)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.server.Client().Do(request)
	if err != nil {
		c.t.Fatalf("do request: %v", err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		c.t.Fatalf("decode response: %v", err)
	}
	return response.StatusCode, decoded
}

func (c *client) createDevice(tag string) string {
	c.t.Helper()
	status, body := c.do(http.MethodPost, "/devices", map[string]any{
		"asset_tag": tag,
		"model":     "Bench Meter",
		"room":      "Room A",
		"steward":   "Lin",
	})
	if status != http.StatusCreated {
		c.t.Fatalf("create device status = %d: %v", status, body)
	}
	device, _ := body["device"].(map[string]any)
	id, _ := device["id"].(string)
	if id == "" {
		c.t.Fatalf("missing device id: %v", body)
	}
	return id
}

func (c *client) openCase(deviceID string) (int, map[string]any) {
	return c.do(http.MethodPost, "/cases", map[string]any{
		"device_id": deviceID,
		"nominal":   25.0,
		"tolerance": 0.5,
		"steward":   "Qiao",
	})
}

func TestSealedDeviceAcceptsNewCaseOverHTTP(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	deviceID := c.createDevice("MET-3001")

	status, body := c.openCase(deviceID)
	if status != http.StatusCreated {
		t.Fatalf("open case status = %d: %v", status, body)
	}
	caseData, _ := body["case"].(map[string]any)
	caseID, _ := caseData["id"].(string)

	status, body = c.do(http.MethodPost, "/cases/"+caseID+"/measurements", map[string]any{
		"observed":   25.2,
		"technician": "Rui",
	})
	if status != http.StatusCreated {
		t.Fatalf("measurement status = %d: %v", status, body)
	}

	status, body = c.do(http.MethodPost, "/cases/"+caseID+"/seal", map[string]any{
		"reviewer": "Verifier",
		"summary":  "accepted measurement sealed for review",
	})
	if status != http.StatusCreated {
		t.Fatalf("seal status = %d: %v", status, body)
	}

	status, body = c.openCase(deviceID)
	if status != http.StatusCreated {
		t.Fatalf("reopen after seal status = %d: %v", status, body)
	}
	newCase, _ := body["case"].(map[string]any)
	if newCase["id"] == caseID {
		t.Fatal("reopen reused the sealed case id")
	}
	if state, _ := newCase["state"].(string); state != "awaiting_measurement" {
		t.Fatalf("new case state = %v", state)
	}

	status, body = c.do(http.MethodGet, "/cases/"+caseID, nil)
	if status != http.StatusOK {
		t.Fatalf("read sealed case status = %d: %v", status, body)
	}
}

func TestInvalidOpenThenValidOpenOverHTTP(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	deviceID := c.createDevice("MET-3002")

	status, body := c.do(http.MethodPost, "/cases", map[string]any{
		"device_id": deviceID,
		"nominal":   25.0,
		"tolerance": 0,
		"steward":   "Qiao",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid open status = %d: %v", status, body)
	}

	status, body = c.openCase(deviceID)
	if status != http.StatusCreated {
		t.Fatalf("valid open after rejected open status = %d: %v", status, body)
	}
}

func TestConcurrentOpenOverHTTPSingleWinner(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	deviceID := c.createDevice("MET-3003")

	const contenders = 8
	start := make(chan struct{})
	statuses := make([]int, contenders)
	var wait sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			statuses[index], _ = c.openCase(deviceID)
		}(i)
	}
	close(start)
	wait.Wait()

	created, conflicts := 0, 0
	for _, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent open status %d", status)
		}
	}
	if created != 1 || conflicts != contenders-1 {
		t.Fatalf("concurrent open outcomes: %d created, %d conflicts", created, conflicts)
	}
}
