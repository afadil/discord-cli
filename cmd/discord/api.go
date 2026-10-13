package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	apiBase         = "https://discord.com/api/v10"
	userAgent       = "DiscordBot (https://github.com/afadil/discord-cli, 0.1.0)"
	keychainService = "discord-cli"
	keychainAccount = "bot-token"
)

// cdnHosts are the only hosts attachments are downloaded from. Attachment
// URLs are signed, so downloads never carry the bot token.
var cdnHosts = map[string]bool{
	"cdn.discordapp.com":   true,
	"media.discordapp.net": true,
}

type client struct {
	base     string
	token    string
	http     *http.Client
	cdnHosts map[string]bool
}

func newClient(token string) *client {
	return &client{
		base:     apiBase,
		token:    token,
		http:     &http.Client{Timeout: 60 * time.Second},
		cdnHosts: cdnHosts,
	}
}

// loadToken prefers the Keychain so a stale DISCORD_TOKEN exported by a shell
// profile cannot shadow the current token.
func loadToken() (string, error) {
	out, err := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", keychainService, "-a", keychainAccount, "-w").Output()
	if err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t, nil
		}
	}
	if t := strings.TrimSpace(os.Getenv("DISCORD_TOKEN")); t != "" {
		return t, nil
	}
	return "", errors.New("no bot token found. Either the Keychain has no \"" + keychainService + "\" entry, " +
		"or this command runs in a sandbox that blocks the Keychain and network (in Codex, run it outside the sandbox). " +
		"To store the token:\n  " + keychainAddCommand)
}

const keychainAddCommand = "security add-generic-password -U -s " + keychainService + " -a " + keychainAccount + " -w"

type apiError struct {
	Status     int
	Code       int     `json:"code"`
	Message    string  `json:"message"`
	RetryAfter float64 `json:"retry_after"`
}

func (e *apiError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "Discord rejected the bot token (401). If it was reset, store the new one:\n  " + keychainAddCommand
	case http.StatusForbidden:
		return fmt.Sprintf("missing access (403, code %d): %s. The bot can't see or post in this channel.", e.Code, e.Message)
	case http.StatusNotFound:
		return fmt.Sprintf("not found (404, code %d): %s", e.Code, e.Message)
	case http.StatusTooManyRequests:
		return fmt.Sprintf("rate limited by Discord; retry after %.1fs", e.RetryAfter)
	}
	return fmt.Sprintf("Discord returned %d (code %d): %s", e.Status, e.Code, e.Message)
}

// call sends one REST request and returns the raw response body. It never
// retries, so a write is sent at most once.
func (c *client) call(method, path string, query url.Values, body any) ([]byte, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bot "+c.token)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		apiErr := &apiError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, apiErr)
		if apiErr.Message == "" {
			apiErr.Message = http.StatusText(resp.StatusCode)
		}
		return nil, apiErr
	}
	return data, nil
}

// maxRetryWait caps how long a read waits out a rate limit before giving up.
const maxRetryWait = 10 * time.Second

// get reads one resource. Discord rejects a rate-limited request without
// processing it, so a read waits out a short limit and tries again; some
// routes, like the server list, allow about one request per second.
func (c *client) get(path string, query url.Values, out any) ([]byte, error) {
	var data []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		data, err = c.call(http.MethodGet, path, query, nil)
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
			break
		}
		wait := time.Duration(apiErr.RetryAfter * float64(time.Second))
		if wait > maxRetryWait {
			break
		}
		time.Sleep(wait + 50*time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return data, nil
}

// download saves a signed CDN attachment to dest without sending the token.
func (c *client) download(rawURL, dest string, maxBytes int64) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || !c.cdnHosts[u.Host] {
		return fmt.Errorf("refusing to download from %q", rawURL)
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	closeErr := f.Close()
	if copyErr == nil && n > maxBytes {
		copyErr = fmt.Errorf("larger than %s", formatSize(maxBytes))
	}
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		os.Remove(dest)
		return copyErr
	}
	return nil
}

// Discord object subsets. Only the fields the CLI prints are decoded; --json
// prints the raw responses instead.

type user struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

type guild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type forumTag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type threadMetadata struct {
	Archived         bool      `json:"archived"`
	Locked           bool      `json:"locked"`
	ArchiveTimestamp time.Time `json:"archive_timestamp"`
}

type channel struct {
	ID             string          `json:"id"`
	Type           int             `json:"type"`
	GuildID        string          `json:"guild_id"`
	Name           string          `json:"name"`
	ParentID       string          `json:"parent_id"`
	Position       int             `json:"position"`
	LastMessageID  string          `json:"last_message_id"`
	MessageCount   int             `json:"message_count"`
	ThreadMetadata *threadMetadata `json:"thread_metadata"`
	AppliedTags    []string        `json:"applied_tags"`
	AvailableTags  []forumTag      `json:"available_tags"`
}

type attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

type embedMedia struct {
	URL string `json:"url"`
}

type embed struct {
	Title       string      `json:"title"`
	Description string      `json:"description"`
	URL         string      `json:"url"`
	Image       *embedMedia `json:"image"`
	Thumbnail   *embedMedia `json:"thumbnail"`
	Video       *embedMedia `json:"video"`
}

type reaction struct {
	Count int `json:"count"`
	Emoji struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"emoji"`
}

type pollAnswer struct {
	AnswerID  int `json:"answer_id"`
	PollMedia struct {
		Text string `json:"text"`
	} `json:"poll_media"`
}

type poll struct {
	Question struct {
		Text string `json:"text"`
	} `json:"question"`
	Answers []pollAnswer `json:"answers"`
	Results *struct {
		AnswerCounts []struct {
			ID    int `json:"id"`
			Count int `json:"count"`
		} `json:"answer_counts"`
	} `json:"results"`
}

type message struct {
	ID                string       `json:"id"`
	Type              int          `json:"type"`
	ChannelID         string       `json:"channel_id"`
	Author            user         `json:"author"`
	Content           string       `json:"content"`
	Timestamp         time.Time    `json:"timestamp"`
	EditedTimestamp   *time.Time   `json:"edited_timestamp"`
	Mentions          []user       `json:"mentions"`
	Attachments       []attachment `json:"attachments"`
	Embeds            []embed      `json:"embeds"`
	Reactions         []reaction   `json:"reactions"`
	ReferencedMessage *message     `json:"referenced_message"`
	MessageReference  *struct {
		MessageID string `json:"message_id"`
	} `json:"message_reference"`
	MessageSnapshots []struct {
		Message message `json:"message"`
	} `json:"message_snapshots"`
	Thread       *channel `json:"thread"`
	Poll         *poll    `json:"poll"`
	StickerItems []struct {
		Name string `json:"name"`
	} `json:"sticker_items"`
}

// snowflakeTime returns when a Discord ID was created.
func snowflakeTime(id string) (time.Time, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(n>>22) + 1420070400000), true
}

// allAttachments includes the attachments of forwarded messages.
func allAttachments(m message) []attachment {
	list := append([]attachment(nil), m.Attachments...)
	for _, snap := range m.MessageSnapshots {
		list = append(list, snap.Message.Attachments...)
	}
	return list
}

func snowflakeLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func validID(id string) bool {
	if id == "" || len(id) > 20 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
