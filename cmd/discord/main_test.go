package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testToken = "test-token-value"

type recorded struct {
	Method, Path, Auth, UserAgent, AuditReason string
	Query                                      url.Values
	Body                                       []byte
}

type fakeDiscord struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []recorded
}

func (f *fakeDiscord) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body)) // handlers can still read it
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recorded{
		Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
		UserAgent: r.Header.Get("User-Agent"), AuditReason: r.Header.Get("X-Audit-Log-Reason"),
		Query: r.URL.Query(), Body: body,
	})
}

func (f *fakeDiscord) requestsTo(path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func newFakeDiscord(t *testing.T) (*fakeDiscord, *client) {
	f := &fakeDiscord{t: t}
	mux := http.NewServeMux()
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	cdn := f.server.URL + "/cdn/"

	jsonReply := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		}
	}
	mux.HandleFunc("GET /api/v10/channels/111", jsonReply(`{"id":"111","type":11,"guild_id":"9","name":"App crashes on start","parent_id":"222"}`))
	mux.HandleFunc("GET /api/v10/channels/111/messages", jsonReply(`[
	  {"id":"1002","type":19,"channel_id":"111","author":{"id":"2","username":"bob","global_name":"Bob"},
	   "content":"thanks <@1>!","timestamp":"2026-10-13T18:05:00Z","mentions":[{"id":"1","username":"alice"}],
	   "message_reference":{"message_id":"1001"},
	   "referenced_message":{"id":"1001","type":0,"author":{"id":"1","username":"alice"},"content":"it crashes","timestamp":"2026-10-13T18:00:00Z"}},
	  {"id":"1001","type":0,"channel_id":"111","author":{"id":"1","username":"alice"},
	   "content":"it crashes","timestamp":"2026-10-13T18:00:00Z",
	   "attachments":[
	     {"id":"a1","filename":"../../shot.png","content_type":"image/png","size":4,"url":"`+cdn+`a.png?ex=1","width":10,"height":20},
	     {"id":"a2","filename":"big.mov","content_type":"video/quicktime","size":31457280,"url":"`+cdn+`big.mov"}],
	   "reactions":[{"count":2,"emoji":{"id":null,"name":"👍"}}]}
	]`))
	mux.HandleFunc("GET /cdn/a.png", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "PNG!") })
	mux.HandleFunc("POST /api/v10/channels/111/messages", jsonReply(`{"id":"900","channel_id":"111"}`))

	mux.HandleFunc("GET /api/v10/channels/222", jsonReply(`{"id":"222","type":15,"guild_id":"9","name":"help",
	  "available_tags":[{"id":"t1","name":"bug"}]}`))
	mux.HandleFunc("GET /api/v10/guilds/9/threads/active", jsonReply(`{"threads":[
	  {"id":"501","type":11,"parent_id":"222","name":"Open bug","message_count":3,"last_message_id":"1427000000000000000",
	   "applied_tags":["t1"],"thread_metadata":{"archived":false,"locked":false}},
	  {"id":"502","type":11,"parent_id":"999","name":"Elsewhere","thread_metadata":{"archived":false}},
	  {"id":"504","type":11,"parent_id":"333","name":"Sync question","thread_metadata":{"archived":false}}]}`))
	mux.HandleFunc("GET /api/v10/guilds/9/channels", jsonReply(`[
	  {"id":"333","type":0,"name":"general","parent_id":"10","position":1},
	  {"id":"10","type":4,"name":"Text Channels","position":1},
	  {"id":"222","type":15,"name":"help","parent_id":"10","position":2},
	  {"id":"20","type":5,"name":"announcements","parent_id":"11","position":0},
	  {"id":"11","type":4,"name":"Info","position":0},
	  {"id":"30","type":0,"name":"welcome","position":0},
	  {"id":"40","type":0,"name":"bugs","parent_id":"10","position":0}
	]`))
	mux.HandleFunc("GET /api/v10/channels/222/threads/archived/public", jsonReply(`{"threads":[
	  {"id":"503","type":11,"parent_id":"222","name":"Old question","message_count":7,
	   "thread_metadata":{"archived":true,"locked":false}}],"has_more":false}`))

	// Channel 333 holds 150 messages with IDs 100001..100150; it pages like
	// Discord: at most ?limit, below ?before or just above ?after, newest first.
	mux.HandleFunc("GET /api/v10/channels/333", jsonReply(`{"id":"333","type":0,"guild_id":"9","name":"general"}`))
	mux.HandleFunc("GET /api/v10/channels/333/messages", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		top, bottom := 100150, 100001
		if b := r.URL.Query().Get("before"); b != "" {
			n, _ := strconv.Atoi(b)
			top = n - 1
		}
		if a := r.URL.Query().Get("after"); a != "" {
			n, _ := strconv.Atoi(a)
			bottom = n + 1
			top = min(top, bottom+limit-1)
		}
		var page []string
		for id := top; id >= bottom && len(page) < limit; id-- {
			page = append(page, fmt.Sprintf(`{"id":"%d","type":0,"author":{"id":"1","username":"u"},"content":"m%d","timestamp":"2026-10-13T18:00:00Z"}`, id, id))
		}
		io.WriteString(w, "["+strings.Join(page, ",")+"]")
	})

	// Channel 444 has a forwarded message (content in message_snapshots) and a
	// reply to it.
	mux.HandleFunc("GET /api/v10/channels/444", jsonReply(`{"id":"444","type":0,"guild_id":"9","name":"general"}`))
	mux.HandleFunc("GET /api/v10/channels/444/messages", jsonReply(`[
	  {"id":"2002","type":19,"author":{"id":"3","username":"carol"},"content":"same here","timestamp":"2026-10-13T18:10:00Z",
	   "message_reference":{"message_id":"2001"},
	   "referenced_message":{"id":"2001","type":0,"author":{"id":"2","username":"bob"},"content":"","flags":16384,
	     "message_snapshots":[{"message":{"type":0,"content":"Sync is in Settings","attachments":[]}}]}},
	  {"id":"2001","type":0,"author":{"id":"2","username":"bob"},"content":"","flags":16384,"timestamp":"2026-10-13T18:00:00Z",
	   "message_reference":{"type":1,"message_id":"7"},
	   "message_snapshots":[{"message":{"type":0,"content":"Sync is in Settings",
	     "attachments":[{"id":"f1","filename":"sync.png","content_type":"image/png","size":4,"url":"`+cdn+`a.png?f=1"}]}}]}
	]`))

	var limited sync.Once
	mux.HandleFunc("GET /api/v10/users/@me/guilds", func(w http.ResponseWriter, r *http.Request) {
		hit := false
		limited.Do(func() { hit = true })
		if hit {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"message":"You are being rate limited.","retry_after":0.01,"global":false}`)
			return
		}
		io.WriteString(w, `[{"id":"9","name":"Test Server"}]`)
	})
	// Bans: user 403 outranks the bot, user 429 hits a rate limit, and bulk-ban
	// reports user 456 as not banned.
	mux.HandleFunc("GET /api/v10/guilds/9", jsonReply(`{"id":"9","name":"Test Server"}`))
	mux.HandleFunc("PUT /api/v10/guilds/9/bans/{user}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("user") {
		case "403":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"Missing Permissions","code":50013}`)
		case "429":
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"message":"You are being rate limited.","retry_after":0.01,"global":false}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("DELETE /api/v10/guilds/9/bans/{user}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v10/guilds/9/bulk-ban", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UserIDs []string `json:"user_ids"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var banned, failed []string
		for _, id := range req.UserIDs {
			if id == "456" {
				failed = append(failed, id)
			} else {
				banned = append(banned, id)
			}
		}
		json.NewEncoder(w).Encode(map[string][]string{"banned_users": banned, "failed_users": failed})
	})

	mux.HandleFunc("GET /api/v10/channels/401", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"401: Unauthorized","code":0}`)
	})

	u, _ := url.Parse(f.server.URL)
	c := &client{
		base:     f.server.URL + "/api/v10",
		token:    testToken,
		http:     f.server.Client(),
		cdnHosts: map[string]bool{u.Host: true},
	}
	return f, c
}

func runCLI(c *client, stdin string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errOut, c)
	return code, out.String(), errOut.String()
}

func TestReadPrintsMessagesOldestFirst(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "read", "111")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	first, second := strings.Index(out, "msg 1001"), strings.Index(out, "msg 1002")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("messages not oldest first:\n%s", out)
	}
	for _, want := range []string{
		`thread "App crashes on start" (111), 2 messages, oldest first`,
		"Bob (@bob) · user 2 · msg 1002",
		"alice · user 1 · msg 1001",
		`reply to msg 1001 by alice: "it crashes"`,
		"  thanks @alice!",
		"attachment: ../../shot.png (image/png, 4 B, 10x20) -> " + f.server.URL + "/cdn/a.png?ex=1",
		"reactions: 👍 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	reqs := f.requestsTo("/api/v10/channels/111/messages")
	if len(reqs) != 1 || reqs[0].Auth != "Bot "+testToken || reqs[0].UserAgent != userAgent() {
		t.Fatalf("unexpected message requests: %+v", reqs)
	}
	if len(f.requestsTo("/cdn/a.png")) != 0 {
		t.Fatal("read without --download fetched an attachment")
	}
}

func TestReadDownloadSavesAttachmentsWithoutToken(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "read", "111", "--download")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	dirLine := strings.SplitN(out, "\n", 2)[0]
	dir := strings.TrimPrefix(dirLine, "attachments saved under ")
	if dir == dirLine || !strings.HasPrefix(dir, os.Getenv("TMPDIR")) {
		t.Fatalf("missing download folder line: %q", dirLine)
	}
	saved := filepath.Join(dir, "a1-shot.png")
	data, err := os.ReadFile(saved)
	if err != nil || string(data) != "PNG!" {
		t.Fatalf("attachment not saved at %s: %v %q", saved, err, data)
	}
	if !strings.Contains(out, "-> "+saved) {
		t.Errorf("output does not point at the saved file:\n%s", out)
	}
	if !strings.Contains(out, "big.mov") || !strings.Contains(out, "not downloaded: 30.0 MB is over the 25.0 MB limit") {
		t.Errorf("oversized attachment not reported as skipped:\n%s", out)
	}
	if len(f.requestsTo("/cdn/big.mov")) != 0 {
		t.Error("oversized attachment was requested")
	}
	cdnReqs := f.requestsTo("/cdn/a.png")
	if len(cdnReqs) != 1 || cdnReqs[0].Auth != "" {
		t.Fatalf("CDN download must not carry the token: %+v", cdnReqs)
	}
}

func TestDownloadRefusesNonCDNHosts(t *testing.T) {
	_, c := newFakeDiscord(t)
	dest := filepath.Join(t.TempDir(), "x")
	for _, u := range []string{"https://example.com/a.png", "http://cdn.discordapp.com/a.png", "file:///etc/passwd"} {
		if err := c.download(u, dest, 10); err == nil {
			t.Errorf("download(%q) succeeded", u)
		}
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("refused download left a file behind")
	}
}

func TestReadPagesBackwardsThroughHistory(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "read", "333", "--limit", "150")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	reqs := f.requestsTo("/api/v10/channels/333/messages")
	if len(reqs) != 2 {
		t.Fatalf("want 2 page requests, got %d", len(reqs))
	}
	if reqs[0].Query.Get("limit") != "100" || reqs[0].Query.Get("before") != "" {
		t.Errorf("first page query: %v", reqs[0].Query)
	}
	if reqs[1].Query.Get("limit") != "50" || reqs[1].Query.Get("before") != "100051" {
		t.Errorf("second page query: %v", reqs[1].Query)
	}
	if !strings.Contains(out, "150 messages") {
		t.Errorf("header count wrong:\n%s", out[:200])
	}
	if strings.Index(out, "msg 100001") > strings.Index(out, "msg 100150") {
		t.Error("paged messages not oldest first")
	}
}

func TestReadForumPointsToPosts(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, _, errOut := runCLI(c, "", "read", "222")
	if code != 2 || !strings.Contains(errOut, "discord posts 222") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if len(f.requestsTo("/api/v10/channels/222/messages")) != 0 {
		t.Error("read a forum's messages endpoint")
	}
}

func TestPostsListsOnlyThatForum(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "posts", "222", "--limit", "10")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "Elsewhere") || !strings.Contains(out, "2 posts") {
		t.Errorf("posts not filtered to the forum:\n%s", out)
	}
	open, archived := strings.Index(out, "Open bug"), strings.Index(out, "Old question")
	if open < 0 || archived < 0 || open > archived {
		t.Errorf("open posts should come first:\n%s", out)
	}
	if !strings.Contains(out, "bug") || !strings.Contains(out, "archived") {
		t.Errorf("tags or state missing:\n%s", out)
	}
	if q := f.requestsTo("/api/v10/channels/222/threads/archived/public"); len(q) != 1 || q[0].Query.Get("limit") != "10" {
		t.Errorf("archived request: %+v", q)
	}
}

func TestSendDisablesPingsAndReplies(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "hello @everyone\n", "send", "111", "--reply-to", "1001")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "sent message 900 to channel 111") {
		t.Errorf("unexpected output %q", out)
	}
	posts := f.requestsTo("/api/v10/channels/111/messages")
	if len(posts) != 1 || posts[0].Method != http.MethodPost {
		t.Fatalf("want one POST, got %+v", posts)
	}
	var body struct {
		Content         string `json:"content"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
		MessageReference struct {
			MessageID string `json:"message_id"`
		} `json:"message_reference"`
	}
	if err := json.Unmarshal(posts[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Content != "hello @everyone" || body.MessageReference.MessageID != "1001" {
		t.Errorf("unexpected body %s", posts[0].Body)
	}
	if !bytes.Contains(posts[0].Body, []byte(`"allowed_mentions":{"parse":[]}`)) || len(body.AllowedMentions.Parse) != 0 {
		t.Errorf("mentions not disabled: %s", posts[0].Body)
	}
}

func TestInvalidInputSendsNothing(t *testing.T) {
	f, c := newFakeDiscord(t)
	cases := [][]string{
		{"send", "111", strings.Repeat("x", maxMessageLength+1)},
		{"send", "111"},
		{"send", "../111", "hi"},
		{"read", "111/../222"},
		{"read", "111", "--before", "1", "--after", "2"},
		{"read", "111", "--limit", "501"},
		{"posts", "abc"},
		{"ban"},
		{"ban", "@spammer"},
		{"ban", "123", "--delete-hours", "169"},
		{"ban", "123", "--delete-hours", "-1"},
		{"ban", "123", "--reason", strings.Repeat("x", maxAuditReason+1)},
		{"ban", "123", "--json"},
		append([]string{"ban"}, manyIDs(maxBanUsers+1)...),
		{"unban"},
		{"unban", "123", "456"},
		{"unban", "../123"},
	}
	for _, args := range cases {
		if code, _, _ := runCLI(c, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if n := len(f.requests); n != 0 {
		t.Fatalf("invalid input made %d requests", n)
	}
}

func TestReadWaitsOutShortRateLimit(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "servers")
	if code != 0 || !strings.Contains(out, "9  Test Server") {
		t.Fatalf("exit %d, out %q, stderr %q", code, out, errOut)
	}
	if n := len(f.requestsTo("/api/v10/users/@me/guilds")); n != 2 {
		t.Fatalf("want 1 retry after the 429, got %d requests", n)
	}
}

func TestChannelsPrintsTreeByCategoryAndPosition(t *testing.T) {
	_, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "channels")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	order := []string{"Test Server (9)", "#welcome", "INFO", "#announcements", "TEXT CHANNELS", "#bugs", "#general", "> Sync question", "#help"}
	last := -1
	for _, want := range order {
		i := strings.Index(out, want)
		if i <= last {
			t.Fatalf("%q missing or out of order:\n%s", want, out)
		}
		last = i
	}
	if !strings.Contains(out, "1 open post\n") || strings.Contains(out, "Elsewhere") || strings.Contains(out, "Open bug") {
		t.Errorf("forum should show a post count, not its posts or other threads:\n%s", out)
	}
}

func TestReadPagesForwardFromAfter(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "read", "333", "--after", "100000", "--limit", "150")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	reqs := f.requestsTo("/api/v10/channels/333/messages")
	if len(reqs) != 2 || reqs[0].Query.Get("after") != "100000" || reqs[1].Query.Get("after") != "100100" || reqs[1].Query.Get("limit") != "50" {
		t.Fatalf("unexpected page queries: %+v", reqs)
	}
	if !strings.Contains(out, "150 messages") || strings.Index(out, "msg 100001") > strings.Index(out, "msg 100150") {
		t.Errorf("forward pages not merged oldest first:\n%s", out[:300])
	}
}

func TestVersionNeedsNoToken(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "")
	old := version
	version = "v9.9.9"
	t.Cleanup(func() { version = old })
	for _, arg := range []string{"version", "--version"} {
		code, out, errOut := runCLI(nil, "", arg)
		if code != 0 || out != "discord v9.9.9\n" || errOut != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", arg, code, out, errOut)
		}
	}
	if !strings.Contains(userAgent(), "v9.9.9") {
		t.Errorf("User-Agent %q does not carry the version", userAgent())
	}
}

func TestHelpNeedsNoToken(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "")
	for _, args := range [][]string{{"read", "-h"}, {"send", "123", "--help"}, {"help"}} {
		// A nil client makes run load the token; help must return before that.
		code, out, errOut := runCLI(nil, "", args...)
		if code != 0 || !strings.Contains(out, "Usage:") || errOut != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

func TestDoubleDashEndsFlags(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, _, errOut := runCLI(c, "", "send", "111", "--", "-5%", "today", "-x")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	posts := f.requestsTo("/api/v10/channels/111/messages")
	if len(posts) != 1 || !bytes.Contains(posts[0].Body, []byte(`"content":"-5% today -x"`)) {
		t.Fatalf("unexpected send: %+v", posts)
	}
}

func TestJSONWithDownloadIsRejected(t *testing.T) {
	f, c := newFakeDiscord(t)
	if code, _, errOut := runCLI(c, "", "read", "111", "--json", "--download"); code != 2 || !strings.Contains(errOut, "--download") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if len(f.requests) != 0 {
		t.Fatal("rejected command made requests")
	}
}

func TestUnauthorizedExplainsTokenReset(t *testing.T) {
	_, c := newFakeDiscord(t)
	code, _, errOut := runCLI(c, "", "read", "401")
	if code != 1 || !strings.Contains(errOut, newTokenHint()) {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if strings.Contains(errOut, testToken) {
		t.Fatal("error output contains the token")
	}
}

func TestReadShowsForwardedMessages(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "read", "444", "--download")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"  forwarded message:\n    Sync is in Settings\n    attachment: sync.png",
		"f1-sync.png",
		`reply to msg 2001 by bob: forwarded "Sync is in Settings"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if len(f.requestsTo("/cdn/a.png")) != 1 {
		t.Error("forwarded attachment was not downloaded")
	}
}

func TestReplyToAttachmentOnlyMessage(t *testing.T) {
	var out bytes.Buffer
	printMessage(&out, message{
		ID: "2", Type: 19, Author: user{Username: "a"},
		MessageReference: &struct {
			MessageID string `json:"message_id"`
		}{MessageID: "1"},
		ReferencedMessage: &message{ID: "1", Author: user{Username: "b"}, Attachments: []attachment{{ID: "x"}}},
	}, nil, nil)
	if !strings.Contains(out.String(), "reply to msg 1 by b: (attachment only)") {
		t.Fatalf("got:\n%s", out.String())
	}
}

func TestSafeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"../../shot.png":  "shot.png",
		".hidden":         "hidden",
		"a b/c:d.txt":     "c_d.txt",
		"..":              "file",
		"screen shot.png": "screen_shot.png",
		"日本.png":          "__.png",
		"/":               "file",
	} {
		if got := safeFilename(in); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func manyIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = strconv.Itoa(1000 + i)
	}
	return ids
}

func TestBanOneUserDeletesMessagesAndLogsReason(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "ban", "123", "--delete-hours", "24", "--reason", "scam links: free nitro ✓")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != "banned user 123 from Test Server (9), deleting their messages from the last 24 hours\n" {
		t.Errorf("unexpected output %q", out)
	}
	reqs := f.requestsTo("/api/v10/guilds/9/bans/123")
	if len(reqs) != 1 || reqs[0].Method != http.MethodPut || reqs[0].Auth != "Bot "+testToken {
		t.Fatalf("want one authorized PUT, got %+v", reqs)
	}
	if string(reqs[0].Body) != `{"delete_message_seconds":86400}` {
		t.Errorf("unexpected body %s", reqs[0].Body)
	}
	if reqs[0].AuditReason != "scam%20links:%20free%20nitro%20%E2%9C%93" {
		t.Errorf("audit reason not percent-encoded: %q", reqs[0].AuditReason)
	}
	if len(f.requestsTo("/api/v10/guilds/9/bulk-ban")) != 0 {
		t.Error("a single ban used bulk-ban")
	}
}

func TestBanWithoutCleanupKeepsMessages(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "ban", "--server", "9", "123")
	if code != 0 || out != "banned user 123 from Test Server (9)\n" {
		t.Fatalf("exit %d, out %q, stderr %q", code, out, errOut)
	}
	reqs := f.requestsTo("/api/v10/guilds/9/bans/123")
	if len(reqs) != 1 || string(reqs[0].Body) != `{"delete_message_seconds":0}` || reqs[0].AuditReason != "" {
		t.Fatalf("unexpected ban request: %+v", reqs)
	}
}

func TestBanSeveralUsersUsesBulkBan(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "ban", "123", "456", "789", "123", "--delete-hours", "1")
	if code != 1 {
		t.Fatalf("exit %d, want 1 when a user was not banned", code)
	}
	if out != "banned 2 users from Test Server (9), deleting their messages from the last 1 hour: 123, 789\n" {
		t.Errorf("unexpected output %q", out)
	}
	if !strings.Contains(errOut, "not banned: 456") {
		t.Errorf("failed user not reported: %q", errOut)
	}
	reqs := f.requestsTo("/api/v10/guilds/9/bulk-ban")
	if len(reqs) != 1 {
		t.Fatalf("want one bulk-ban request, got %d", len(reqs))
	}
	var body struct {
		UserIDs []string `json:"user_ids"`
		Seconds int      `json:"delete_message_seconds"`
	}
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if strings.Join(body.UserIDs, ",") != "123,456,789" || body.Seconds != 3600 {
		t.Errorf("unexpected bulk-ban body %s", reqs[0].Body)
	}
	for _, r := range f.requests {
		if r.Method == http.MethodPut {
			t.Errorf("bulk ban also sent a single ban: %s", r.Path)
		}
	}
}

func TestBanExplainsMissingPermission(t *testing.T) {
	_, c := newFakeDiscord(t)
	code, _, errOut := runCLI(c, "", "ban", "403")
	if code != 1 || !strings.Contains(errOut, "Missing Permissions") || !strings.Contains(errOut, "Ban Members") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if strings.Contains(errOut, "channel") {
		t.Errorf("ban error talks about channels: %q", errOut)
	}
}

func TestBanNeverRetries(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, _, errOut := runCLI(c, "", "ban", "429")
	if code != 1 || !strings.Contains(errOut, "rate limited") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if n := len(f.requestsTo("/api/v10/guilds/9/bans/429")); n != 1 {
		t.Fatalf("ban sent %d times, want 1", n)
	}
}

func TestUnban(t *testing.T) {
	f, c := newFakeDiscord(t)
	code, out, errOut := runCLI(c, "", "unban", "123", "--reason", "false positive")
	if code != 0 || !strings.HasPrefix(out, "unbanned user 123 in Test Server (9)") {
		t.Fatalf("exit %d, out %q, stderr %q", code, out, errOut)
	}
	reqs := f.requestsTo("/api/v10/guilds/9/bans/123")
	if len(reqs) != 1 || reqs[0].Method != http.MethodDelete || reqs[0].AuditReason != "false%20positive" {
		t.Fatalf("unexpected unban requests: %+v", reqs)
	}
}

func TestWebhookMessagesShowNoUserID(t *testing.T) {
	var out bytes.Buffer
	printMessage(&out, message{ID: "5", WebhookID: "77", Author: user{ID: "77", Username: "GitHub", Bot: true}}, nil, nil)
	if !strings.Contains(out.String(), "GitHub [bot] · via webhook · msg 5") || strings.Contains(out.String(), "user 77") {
		t.Fatalf("got:\n%s", out.String())
	}
}
