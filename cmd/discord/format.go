package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	channelText               = 0
	channelVoice              = 2
	channelCategory           = 4
	channelAnnouncement       = 5
	channelAnnouncementThread = 10
	channelPublicThread       = 11
	channelPrivateThread      = 12
	channelStage              = 13
	channelForum              = 15
	channelMedia              = 16
)

func channelKind(t int) string {
	switch t {
	case channelText:
		return "text"
	case channelVoice:
		return "voice"
	case channelCategory:
		return "category"
	case channelAnnouncement:
		return "announcement"
	case channelAnnouncementThread, channelPublicThread:
		return "thread"
	case channelPrivateThread:
		return "private thread"
	case channelStage:
		return "stage"
	case channelForum:
		return "forum"
	case channelMedia:
		return "media"
	}
	return fmt.Sprintf("type %d", t)
}

func isThread(t int) bool {
	return t == channelAnnouncementThread || t == channelPublicThread || t == channelPrivateThread
}

// printChannelTree prints channels by category and position. Text channels list
// their active threads; forums only show a count, since `discord posts` lists
// them.
func printChannelTree(out io.Writer, channels, threads []channel) {
	byPosition := func(list []channel) {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Position != list[j].Position {
				return list[i].Position < list[j].Position
			}
			return snowflakeLess(list[i].ID, list[j].ID)
		})
	}
	threadsByParent := map[string][]channel{}
	for _, t := range threads {
		threadsByParent[t.ParentID] = append(threadsByParent[t.ParentID], t)
	}
	var categories []channel
	children := map[string][]channel{}
	for _, ch := range channels {
		if ch.Type == channelCategory {
			categories = append(categories, ch)
		} else {
			children[ch.ParentID] = append(children[ch.ParentID], ch)
		}
	}
	byPosition(categories)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	printGroup := func(indent string, list []channel) {
		byPosition(list)
		for _, ch := range list {
			active := threadsByParent[ch.ID]
			note := ""
			if (ch.Type == channelForum || ch.Type == channelMedia) && len(active) > 0 {
				note = fmt.Sprintf("%d open post", len(active))
				if len(active) != 1 {
					note += "s"
				}
			}
			fmt.Fprintf(tw, "%s#%s\t%s\t%s\t%s\n", indent, ch.Name, channelKind(ch.Type), ch.ID, note)
			if ch.Type == channelForum || ch.Type == channelMedia {
				continue
			}
			for _, t := range active {
				fmt.Fprintf(tw, "%s  > %s\t%s\t%s\t\n", indent, t.Name, channelKind(t.Type), t.ID)
			}
		}
	}
	printGroup("", children[""])
	for _, cat := range categories {
		fmt.Fprintf(tw, "%s\t\t\t\n", strings.ToUpper(cat.Name))
		printGroup("  ", children[cat.ID])
	}
	tw.Flush()
}

func printPosts(out io.Writer, forum channel, posts []channel) {
	tags := map[string]string{}
	for _, t := range forum.AvailableTags {
		tags[t.ID] = t.Name
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLAST ACTIVITY\tMSGS\tSTATE\tTITLE\tTAGS")
	for _, p := range posts {
		state := "open"
		if p.ThreadMetadata != nil {
			if p.ThreadMetadata.Locked {
				state = "locked"
			} else if p.ThreadMetadata.Archived {
				state = "archived"
			}
		}
		last := "-"
		if t, ok := snowflakeTime(p.LastMessageID); ok {
			last = formatTime(t)
		}
		var names []string
		for _, id := range p.AppliedTags {
			if n, ok := tags[id]; ok {
				names = append(names, n)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", p.ID, last, p.MessageCount, state, p.Name, strings.Join(names, ", "))
	}
	tw.Flush()
}

func printChannelHeader(out io.Writer, ch channel, count int) {
	kind := channelKind(ch.Type)
	if isThread(ch.Type) {
		fmt.Fprintf(out, "%s %q (%s), %d messages, oldest first\n", kind, ch.Name, ch.ID, count)
		return
	}
	fmt.Fprintf(out, "#%s (%s, %s), %d messages, oldest first\n", ch.Name, kind, ch.ID, count)
}

var systemMessages = map[int]string{
	6:  "pinned a message",
	7:  "joined the server",
	18: "started a thread",
	46: "poll ended",
}

func printMessage(out io.Writer, m message, downloads, skipped map[string]string) {
	// The author's user ID is what `discord ban` takes. A webhook's author ID
	// is not a user, so it is not printed.
	header := fmt.Sprintf("[%s] %s", formatTime(m.Timestamp), displayName(m.Author))
	if m.WebhookID != "" {
		header += " · via webhook"
	} else if m.Author.ID != "" {
		header += " · user " + m.Author.ID
	}
	header += " · msg " + m.ID
	if m.EditedTimestamp != nil {
		header += " (edited)"
	}
	fmt.Fprintln(out, header)

	switch m.Type {
	case 0, 19, 20, 21, 23:
	default:
		label, ok := systemMessages[m.Type]
		if !ok {
			label = fmt.Sprintf("system message type %d", m.Type)
		}
		fmt.Fprintf(out, "  (%s)\n", label)
	}
	if m.MessageReference != nil && m.MessageReference.MessageID != "" && m.Type == 19 {
		if r := m.ReferencedMessage; r != nil {
			fmt.Fprintf(out, "  reply to msg %s by %s: %s\n", r.ID, displayName(r.Author), preview(*r))
		} else {
			fmt.Fprintf(out, "  reply to msg %s (deleted)\n", m.MessageReference.MessageID)
		}
	}
	printBody(out, m, "  ", downloads, skipped)
	for _, snap := range m.MessageSnapshots {
		fmt.Fprintln(out, "  forwarded message:")
		printBody(out, snap.Message, "    ", downloads, skipped)
	}
	if len(m.Reactions) > 0 {
		parts := make([]string, len(m.Reactions))
		for i, r := range m.Reactions {
			name := r.Emoji.Name
			if r.Emoji.ID != "" {
				name = ":" + name + ":"
			}
			parts[i] = fmt.Sprintf("%s %d", name, r.Count)
		}
		fmt.Fprintf(out, "  reactions: %s\n", strings.Join(parts, ", "))
	}
	if m.Thread != nil {
		fmt.Fprintf(out, "  thread: %q (%s), read it with `discord read %s`\n", m.Thread.Name, m.Thread.ID, m.Thread.ID)
	}
}

// printBody prints what a message carries: text, attachments, embeds, polls
// and stickers. Forwarded messages reuse it for their snapshot.
func printBody(out io.Writer, m message, indent string, downloads, skipped map[string]string) {
	if content := resolveMentions(m.Content, m.Mentions); content != "" {
		for _, line := range strings.Split(content, "\n") {
			fmt.Fprintln(out, indent+line)
		}
	}
	for _, a := range m.Attachments {
		details := []string{}
		if a.ContentType != "" {
			details = append(details, a.ContentType)
		}
		details = append(details, formatSize(a.Size))
		if a.Width > 0 && a.Height > 0 {
			details = append(details, fmt.Sprintf("%dx%d", a.Width, a.Height))
		}
		where := a.URL
		if p, ok := downloads[a.ID]; ok {
			where = p
		} else if reason, ok := skipped[a.ID]; ok {
			where = a.URL + " (" + reason + ")"
		}
		fmt.Fprintf(out, indent+"attachment: %s (%s) -> %s\n", a.Filename, strings.Join(details, ", "), where)
	}
	for _, e := range m.Embeds {
		parts := []string{}
		for _, s := range []string{e.Title, e.URL} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		if e.Description != "" {
			parts = append(parts, excerpt(e.Description, 200))
		}
		for _, media := range []*embedMedia{e.Image, e.Video, e.Thumbnail} {
			if media != nil && media.URL != "" {
				parts = append(parts, "media "+media.URL)
				break
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(out, indent+"embed: %s\n", strings.Join(parts, " | "))
		}
	}
	if m.Poll != nil {
		counts := map[int]int{}
		if m.Poll.Results != nil {
			for _, c := range m.Poll.Results.AnswerCounts {
				counts[c.ID] = c.Count
			}
		}
		fmt.Fprintf(out, indent+"poll: %s\n", m.Poll.Question.Text)
		for _, a := range m.Poll.Answers {
			fmt.Fprintf(out, indent+"  - %s (%d)\n", a.PollMedia.Text, counts[a.AnswerID])
		}
	}
	for _, s := range m.StickerItems {
		fmt.Fprintf(out, indent+"sticker: %s\n", s.Name)
	}
}

// preview summarizes a replied-to message in one line.
func preview(m message) string {
	if text := excerpt(resolveMentions(m.Content, m.Mentions), 80); text != "" {
		return fmt.Sprintf("%q", text)
	}
	for _, snap := range m.MessageSnapshots {
		if text := excerpt(resolveMentions(snap.Message.Content, snap.Message.Mentions), 80); text != "" {
			return fmt.Sprintf("forwarded %q", text)
		}
	}
	if len(allAttachments(m)) > 0 {
		return "(attachment only)"
	}
	return "(no text)"
}

func displayName(u user) string {
	name := u.Username
	if u.GlobalName != "" && u.GlobalName != u.Username {
		name = u.GlobalName + " (@" + u.Username + ")"
	}
	if u.Bot {
		name += " [bot]"
	}
	return name
}

// resolveMentions swaps <@id> user mentions for readable @names.
func resolveMentions(content string, mentions []user) string {
	for _, u := range mentions {
		name := "@" + u.Username
		content = strings.ReplaceAll(content, "<@"+u.ID+">", name)
		content = strings.ReplaceAll(content, "<@!"+u.ID+">", name)
	}
	return content
}

func excerpt(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func formatTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04 MST")
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
