package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/agent"
)

type fakeWikiAgent struct {
	calls   int
	roots   []string
	history []int
}

func (r *fakeWikiAgent) RunWithHistory(_ context.Context, root string, question string, history []*schema.Message) (*agent.Result, error) {
	r.calls++
	r.roots = append(r.roots, root)
	r.history = append(r.history, len(history))
	return &agent.Result{Messages: append(history, schema.UserMessage(question), schema.AssistantMessage("回答："+question, nil)), Answer: "回答：" + question}, nil
}

func TestChatSessionAndPage(t *testing.T) {
	wikiAgent := &fakeWikiAgent{}
	srv := httptest.NewServer(New(wikiAgent).Handler())
	defer srv.Close()
	page, err := http.Get(srv.URL + "/")
	if err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("page: %v", err)
	}
	page.Body.Close()

	client := &http.Client{}
	project, err := client.Get(srv.URL + "/api/project")
	if err != nil {
		t.Fatal(err)
	}
	defer project.Body.Close()
	var current projectResponse
	if err := json.NewDecoder(project.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if current.Root != "" {
		t.Fatalf("project=%+v", current)
	}
	request := func(message string, cookie string) (*http.Response, chatResponse) {
		body := strings.NewReader(`{"message":"` + message + `"}`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/chat", body)
		req.Header.Set("Content-Type", "application/json")
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result chatResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return resp, result
	}
	blocked, result := request("未选目录", "")
	if blocked.StatusCode != http.StatusBadRequest || !strings.Contains(result.Error, "打开") || len(blocked.Cookies()) != 1 {
		t.Fatalf("blocked=%+v status=%d", result, blocked.StatusCode)
	}
	cookie := blocked.Cookies()[0].Name + "=" + blocked.Cookies()[0].Value

	openBody := strings.NewReader(`{"root":"/notes/project-a"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/project", openBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	opened, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Body.Close()
	if opened.StatusCode != http.StatusOK {
		t.Fatalf("opened=%d", opened.StatusCode)
	}

	first, result := request("第一问", cookie)
	if first.StatusCode != http.StatusOK || result.Answer != "回答：第一问" || wikiAgent.roots[0] != "/notes/project-a" {
		t.Fatalf("first=%+v", result)
	}
	second, result := request("追问", cookie)
	if second.StatusCode != http.StatusOK || result.Answer != "回答：追问" || wikiAgent.calls != 2 || wikiAgent.history[1] != 2 {
		t.Fatalf("second=%+v histories=%v", result, wikiAgent.history)
	}

	openBody = strings.NewReader(`{"root":"/notes/project-b"}`)
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/project", openBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	opened, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Body.Close()
	if opened.StatusCode != http.StatusOK {
		t.Fatalf("opened=%d", opened.StatusCode)
	}
	third, result := request("新目录问题", cookie)
	if third.StatusCode != http.StatusOK || result.Answer == "" || wikiAgent.roots[2] != "/notes/project-b" || wikiAgent.history[2] != 0 {
		t.Fatalf("third=%+v roots=%v histories=%v", result, wikiAgent.roots, wikiAgent.history)
	}
}

func TestChatValidation(t *testing.T) {
	srv := httptest.NewServer(New(&fakeWikiAgent{}).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"message":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	resp, err = http.Post(srv.URL+"/api/project", "application/json", strings.NewReader(`{"root":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("project status=%d", resp.StatusCode)
	}
}
