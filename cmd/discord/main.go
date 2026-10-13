// Command discord reads and posts Discord messages as a bot through the REST
// API. It never opens a gateway connection, so running it any number of times
// does not count toward the bot's daily login (IDENTIFY) limit.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode/utf8"
)

const (
	maxMessageLength = 2000
	maxReadLimit     = 500
	maxDownloadBytes = 25 << 20
)

const usage = `discord: read and post Discord messages as the bot (REST only, no gateway).

Usage:
  discord servers                                 servers the bot is in
  discord channels [--server ID]                  channels and active threads
  discord posts FORUM_ID [--limit N]              all open forum posts plus the N
                                                  most recently archived (default 25)
  discord read CHANNEL_ID [--limit N] [--before MSG_ID | --after MSG_ID] [--download]
                                                  messages, oldest first; a forum
                                                  post or thread ID works here
  discord send CHANNEL_ID [--reply-to MSG_ID] [TEXT]
                                                  post TEXT, or stdin when TEXT is
                                                  omitted; never pings anyone

Flags:
  --json        print the raw Discord JSON instead of text
  --download    save attachments to a new temp folder and print their paths

The bot token is read from the Keychain (service "discord-cli", account
"bot-token"), falling back to DISCORD_TOKEN. Store it with:
  ` + keychainAddCommand + `
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, nil))
}

// run executes one command. c is nil in production; tests pass a client
// pointed at a fake Discord.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, c *client) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	for _, a := range rest {
		if a == "--" {
			break
		}
		if a == "-h" || a == "-help" || a == "--help" {
			fmt.Fprint(stdout, usage)
			return 0
		}
	}
	commands := map[string]func(*client, []string, io.Reader, io.Writer) error{
		"servers":  cmdServers,
		"channels": cmdChannels,
		"posts":    cmdPosts,
		"read":     cmdRead,
		"send":     cmdSend,
	}
	fn, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if c == nil {
		token, err := loadToken()
		if err != nil {
			fmt.Fprintln(stderr, "discord:", err)
			return 1
		}
		c = newClient(token)
	}
	if err := fn(c, rest, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "discord:", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

type usageError string

func (e usageError) Error() string { return string(e) }

// parseArgs parses flags that may appear before or after positional
// arguments. Everything after "--" is positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError(err.Error())
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		// Parse stops at the first non-flag argument, or consumes "--" and
		// stops right after it.
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func requireID(kind, id string) error {
	if !validID(id) {
		return usageError(fmt.Sprintf("%s must be a numeric Discord ID, got %q", kind, id))
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdServers(c *client, args []string, _ io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("servers", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	var guilds []guild
	raw, err := c.get("/users/@me/guilds", nil, &guilds)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := out.Write(raw)
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, g := range guilds {
		fmt.Fprintf(tw, "%s\t%s\n", g.ID, g.Name)
	}
	return tw.Flush()
}

// resolveGuild defaults to the bot's only server when --server is omitted.
func resolveGuild(c *client, id string) (guild, error) {
	if id != "" {
		if err := requireID("--server", id); err != nil {
			return guild{}, err
		}
		var g guild
		_, err := c.get("/guilds/"+id, nil, &g)
		return g, err
	}
	var guilds []guild
	if _, err := c.get("/users/@me/guilds", nil, &guilds); err != nil {
		return guild{}, err
	}
	switch len(guilds) {
	case 0:
		return guild{}, errors.New("the bot is not in any server")
	case 1:
		return guilds[0], nil
	}
	names := make([]string, len(guilds))
	for i, g := range guilds {
		names[i] = g.ID + " " + g.Name
	}
	return guild{}, usageError("the bot is in several servers; pass --server with one of:\n  " + strings.Join(names, "\n  "))
}

func cmdChannels(c *client, args []string, _ io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("channels", flag.ContinueOnError)
	server := fs.String("server", "", "")
	asJSON := fs.Bool("json", false, "")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	g, err := resolveGuild(c, *server)
	if err != nil {
		return err
	}
	var channels []channel
	rawChannels, err := c.get("/guilds/"+g.ID+"/channels", nil, &channels)
	if err != nil {
		return err
	}
	var active struct {
		Threads []channel `json:"threads"`
	}
	rawThreads, err := c.get("/guilds/"+g.ID+"/threads/active", nil, &active)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(out, map[string]json.RawMessage{"channels": rawChannels, "active_threads": rawThreads})
	}
	fmt.Fprintf(out, "%s (%s)\n", g.Name, g.ID)
	printChannelTree(out, channels, active.Threads)
	return nil
}

func cmdPosts(c *client, args []string, _ io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("posts", flag.ContinueOnError)
	limit := fs.Int("limit", 25, "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("usage: discord posts FORUM_ID [--limit N]")
	}
	if err := requireID("FORUM_ID", pos[0]); err != nil {
		return err
	}
	if *limit < 1 || *limit > 100 {
		return usageError("--limit must be between 1 and 100")
	}
	var parent channel
	if _, err := c.get("/channels/"+pos[0], nil, &parent); err != nil {
		return err
	}
	var active struct {
		Threads []json.RawMessage `json:"threads"`
	}
	if _, err := c.get("/guilds/"+parent.GuildID+"/threads/active", nil, &active); err != nil {
		return err
	}
	var archived struct {
		Threads []json.RawMessage `json:"threads"`
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}}
	if _, err := c.get("/channels/"+parent.ID+"/threads/archived/public", q, &archived); err != nil {
		return err
	}

	var raws []json.RawMessage
	var posts []channel
	for _, list := range [][]json.RawMessage{active.Threads, archived.Threads} {
		for _, r := range list {
			var t channel
			if err := json.Unmarshal(r, &t); err != nil {
				return err
			}
			if t.ParentID == parent.ID {
				raws = append(raws, r)
				posts = append(posts, t)
			}
		}
	}
	if *asJSON {
		return writeJSON(out, raws)
	}
	fmt.Fprintf(out, "#%s (%s): %d posts, open first\n", parent.Name, parent.ID, len(posts))
	printPosts(out, parent, posts)
	return nil
}

func cmdRead(c *client, args []string, _ io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "")
	before := fs.String("before", "", "")
	after := fs.String("after", "", "")
	download := fs.Bool("download", false, "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("usage: discord read CHANNEL_ID [--limit N] [--before MSG_ID | --after MSG_ID] [--download]")
	}
	id := pos[0]
	if err := requireID("CHANNEL_ID", id); err != nil {
		return err
	}
	if *limit < 1 || *limit > maxReadLimit {
		return usageError(fmt.Sprintf("--limit must be between 1 and %d", maxReadLimit))
	}
	if *before != "" && *after != "" {
		return usageError("pass --before or --after, not both")
	}
	if *asJSON && *download {
		return usageError("--download can't be combined with --json")
	}
	for flagName, v := range map[string]string{"--before": *before, "--after": *after} {
		if v != "" {
			if err := requireID(flagName, v); err != nil {
				return err
			}
		}
	}

	var ch channel
	if _, err := c.get("/channels/"+id, nil, &ch); err != nil {
		return err
	}
	if ch.Type == channelForum || ch.Type == channelMedia {
		return usageError(fmt.Sprintf("#%s is a forum; list its posts with `discord posts %s`, then read a post by its ID", ch.Name, id))
	}

	raws, msgs, err := fetchMessages(c, id, *limit, *before, *after)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(out, raws)
	}

	var downloads map[string]string
	var skipped map[string]string
	if *download {
		downloads, skipped, err = downloadAttachments(c, id, msgs, out)
		if err != nil {
			return err
		}
	}
	printChannelHeader(out, ch, len(msgs))
	for _, m := range msgs {
		fmt.Fprintln(out)
		printMessage(out, m, downloads, skipped)
	}
	return nil
}

// fetchMessages pages through up to limit messages and returns them oldest
// first, both raw and decoded.
func fetchMessages(c *client, id string, limit int, before, after string) ([]json.RawMessage, []message, error) {
	var raws []json.RawMessage
	var msgs []message
	cursor := before
	if after != "" {
		cursor = after
	}
	for len(msgs) < limit {
		n := min(100, limit-len(msgs))
		q := url.Values{"limit": {strconv.Itoa(n)}}
		if after != "" {
			q.Set("after", cursor)
		} else if cursor != "" {
			q.Set("before", cursor)
		}
		var page []json.RawMessage
		if _, err := c.get("/channels/"+id+"/messages", q, &page); err != nil {
			return nil, nil, err
		}
		for _, r := range page {
			var m message
			if err := json.Unmarshal(r, &m); err != nil {
				return nil, nil, err
			}
			raws = append(raws, r)
			msgs = append(msgs, m)
			if after != "" && snowflakeLess(cursor, m.ID) {
				cursor = m.ID
			}
			if after == "" && (cursor == "" || snowflakeLess(m.ID, cursor)) {
				cursor = m.ID
			}
		}
		if len(page) < n {
			break
		}
	}
	order := make([]int, len(msgs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return snowflakeLess(msgs[order[a]].ID, msgs[order[b]].ID) })
	sortedRaws := make([]json.RawMessage, len(order))
	sortedMsgs := make([]message, len(order))
	for i, j := range order {
		sortedRaws[i], sortedMsgs[i] = raws[j], msgs[j]
	}
	return sortedRaws, sortedMsgs, nil
}

// downloadAttachments saves attachments into a fresh temp folder. It returns
// local paths and skip reasons keyed by attachment ID.
func downloadAttachments(c *client, channelID string, msgs []message, out io.Writer) (map[string]string, map[string]string, error) {
	downloads := map[string]string{}
	skipped := map[string]string{}
	var dir string
	for _, m := range msgs {
		for _, a := range allAttachments(m) {
			if a.Size > maxDownloadBytes {
				skipped[a.ID] = fmt.Sprintf("not downloaded: %s is over the %s limit", formatSize(a.Size), formatSize(maxDownloadBytes))
				continue
			}
			if dir == "" {
				var err error
				dir, err = os.MkdirTemp("", "discord-"+channelID+"-")
				if err != nil {
					return nil, nil, err
				}
				fmt.Fprintf(out, "attachments saved under %s\n", dir)
			}
			dest := filepath.Join(dir, a.ID+"-"+safeFilename(a.Filename))
			if err := c.download(a.URL, dest, maxDownloadBytes); err != nil {
				skipped[a.ID] = "not downloaded: " + err.Error()
				continue
			}
			downloads[a.ID] = dest
		}
	}
	return downloads, skipped, nil
}

func safeFilename(name string) string {
	name = filepath.Base(name)
	if name == "/" || name == "." || name == ".." {
		name = ""
	}
	var b strings.Builder
	for _, r := range name {
		if r < utf8.RuneSelf && (r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := strings.TrimLeft(b.String(), ".")
	if s == "" {
		s = "file"
	}
	if len(s) > 100 {
		s = s[len(s)-100:]
	}
	return s
}

func cmdSend(c *client, args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	replyTo := fs.String("reply-to", "", "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return usageError("usage: discord send CHANNEL_ID [--reply-to MSG_ID] [TEXT]")
	}
	id := pos[0]
	if err := requireID("CHANNEL_ID", id); err != nil {
		return err
	}
	if *replyTo != "" {
		if err := requireID("--reply-to", *replyTo); err != nil {
			return err
		}
	}
	text := strings.Join(pos[1:], " ")
	if text == "" {
		b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
		if err != nil {
			return err
		}
		text = string(b)
	}
	text = strings.TrimRight(text, "\r\n")
	if strings.TrimSpace(text) == "" {
		return usageError("nothing to send: pass TEXT or pipe it on stdin")
	}
	if n := utf8.RuneCountInString(text); n > maxMessageLength {
		return usageError(fmt.Sprintf("message is %d characters; Discord allows %d", n, maxMessageLength))
	}

	body := map[string]any{
		"content":          text,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if *replyTo != "" {
		body["message_reference"] = map[string]any{"message_id": *replyTo}
	}
	raw, err := c.call(http.MethodPost, "/channels/"+id+"/messages", nil, body)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := out.Write(raw)
		return err
	}
	var sent message
	if err := json.Unmarshal(raw, &sent); err != nil {
		return err
	}
	fmt.Fprintf(out, "sent message %s to channel %s\n", sent.ID, id)
	return nil
}
