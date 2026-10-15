package history

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CursorCLI reads local Cursor CLI (cursor-agent) chats:
// <home>/chats/<md5 of the cwd>/<agent id>/ holds meta.json (cwd and
// times) and store.db, a SQLite database whose blobs table holds each
// message as JSON ({"role", "content"}) under its sha256, and whose meta
// row "0" (hex-encoded JSON) names the root blob listing the messages in
// order. Each store.db is copied with its -wal and -shm before it is
// opened (withSQLiteCopy).
//
// Wake runs are sessions whose ids a wake recorded in a
// <name>.wake-sessions list in WakeDir, or whose cwd is a wake's workdir
// or home; sessions in ScratchDir are never reported.
type CursorCLI struct {
	// Home is cursor-agent's config dir: $CURSOR_CONFIG_DIR, else
	// $XDG_CONFIG_HOME/cursor, else ~/.cursor.
	Home       string
	WakeDir    string
	ScratchDir string
	// TempDir is where store copies are made; os.TempDir() when empty.
	TempDir string
	Window  Window
	Now     func() time.Time
}

// NewCursorCLI returns a reader for cursor-agent's config dir.
func NewCursorCLI() *CursorCLI {
	home := os.Getenv("CURSOR_CONFIG_DIR")
	if strings.TrimSpace(home) == "" {
		if x := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(x) != "" {
			home = filepath.Join(x, "cursor")
		} else if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".cursor")
		}
	}
	return &CursorCLI{Home: home, WakeDir: configPath("", ""), ScratchDir: DefaultScratchDir()}
}

// Source implements Reader.
func (c *CursorCLI) Source() Source { return SourceCursorCLI }

func (c *CursorCLI) store() cliStore {
	return cliStore{source: SourceCursorCLI, wakeDir: c.WakeDir, scratchDir: c.ScratchDir, window: c.Window, now: c.Now}
}

// candidates returns the sessions newest first.
func (c *CursorCLI) candidates(ctx context.Context) ([]cliSession, error) {
	root := filepath.Join(c.Home, "chats")
	hashes, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, noHistory("Cursor CLI", root)
	}
	if err != nil {
		return nil, fmt.Errorf("cursor-cli: read chats: %w", err)
	}
	var out []cliSession
	for _, h := range hashes {
		if !h.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sessions, _ := os.ReadDir(filepath.Join(root, h.Name()))
		for _, s := range sessions {
			if !s.IsDir() || !idPattern.MatchString(s.Name()) {
				continue
			}
			dir := filepath.Join(root, h.Name(), s.Name())
			db := filepath.Join(dir, "store.db")
			st, err := os.Stat(db)
			if err != nil {
				continue
			}
			e := cliSession{id: s.Name(), updated: st.ModTime()}
			var meta struct {
				CreatedAtMs int64  `json:"createdAtMs"`
				UpdatedAtMs int64  `json:"updatedAtMs"`
				Cwd         string `json:"cwd"`
			}
			if raw, err := ReadCapped(filepath.Join(dir, "meta.json"), 1<<20); err == nil && json.Unmarshal(raw, &meta) == nil {
				e.cwd = meta.Cwd
				if t := msTime(max(meta.UpdatedAtMs, meta.CreatedAtMs)); !t.IsZero() {
					e.updated = t
				}
			}
			updated, tmp := e.updated, c.TempDir
			e.load = func() (turns []turn, title string, err error) {
				err = withSQLiteCopy(db, tmp, func(d *sql.DB) error {
					turns, title, err = cursorCLITurns(d, updated)
					return err
				})
				return turns, title, err
			}
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, noHistory("Cursor CLI", root)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].updated.After(out[j].updated) })
	return out, nil
}

// cursorCLIMessage is one message blob.
type cursorCLIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// texts returns the message's text: a string content, or its text parts.
func (m cursorCLIMessage) texts() []string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []string{s}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(m.Content, &parts)
	var out []string
	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, p.Text)
		}
	}
	return out
}

var (
	cursorQuery     = regexp.MustCompile(`(?s)<user_query>(.*?)</user_query>`)
	cursorTimestamp = regexp.MustCompile(`<timestamp>([^<]*)</timestamp>`)
)

// cursorCLITurns reads one store's messages in the root blob's order. The
// owner's typed prompt is the <user_query> block of a user message;
// user messages without one (the environment block, rules, reminders) are
// context Cursor added. The reply is the last text the assistant gave
// before the next prompt.
func cursorCLITurns(db *sql.DB, updated time.Time) ([]turn, string, error) {
	var metaHex string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = '0'`).Scan(&metaHex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	raw, herr := hex.DecodeString(strings.TrimSpace(metaHex))
	if herr != nil {
		raw = []byte(metaHex)
	}
	var meta struct {
		Name string `json:"name"`
		Root string `json:"latestRootBlobId"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Root == "" {
		return nil, "", nil
	}
	blob := func(id string) ([]byte, error) {
		var b []byte
		err := db.QueryRow(`SELECT data FROM blobs WHERE id = ?`, id).Scan(&b)
		return b, err
	}
	root, err := blob(meta.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, meta.Name, nil
	}
	if err != nil {
		return nil, "", err
	}
	var turns []turn
	for _, id := range cursorRootIDs(root) {
		b, err := blob(id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		var m cursorCLIMessage
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch m.Role {
		case "user":
			for _, t := range m.texts() {
				q := cursorQuery.FindStringSubmatch(t)
				if q == nil || strings.TrimSpace(q[1]) == "" {
					continue
				}
				var at time.Time
				if ts := cursorTimestamp.FindStringSubmatch(t); ts != nil {
					at = parseCursorTimestamp(ts[1])
				}
				turns = append(turns, turn{prompt: Message{Role: RoleUser, Text: strings.TrimSpace(q[1]), Time: at}})
			}
		case "assistant":
			if len(turns) == 0 {
				continue
			}
			if text := strings.TrimSpace(strings.Join(m.texts(), "\n")); text != "" {
				turns[len(turns)-1].reply = Message{Role: RoleAssistant, Text: text}
			}
		}
	}
	for i := range turns {
		if turns[i].prompt.Time.IsZero() {
			turns[i].prompt.Time = updated
		}
	}
	return turns, meta.Name, nil
}

// cursorRootIDs returns the message blob ids a root blob lists: the
// 32-byte values of its protobuf field 1, in order. Other fields are
// skipped; a malformed blob yields what was read before the fault.
func cursorRootIDs(b []byte) []string {
	var out []string
	varint := func() (uint64, bool) {
		var v uint64
		for shift := uint(0); shift < 64; shift += 7 {
			if len(b) == 0 {
				return 0, false
			}
			c := b[0]
			b = b[1:]
			v |= uint64(c&0x7f) << shift
			if c < 0x80 {
				return v, true
			}
		}
		return 0, false
	}
	for len(b) > 0 {
		key, ok := varint()
		if !ok {
			return out
		}
		switch key & 7 {
		case 0:
			if _, ok := varint(); !ok {
				return out
			}
		case 1:
			if len(b) < 8 {
				return out
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return out
			}
			b = b[4:]
		case 2:
			n, ok := varint()
			if !ok || n > uint64(len(b)) {
				return out
			}
			if key>>3 == 1 && n == 32 {
				out = append(out, hex.EncodeToString(b[:32]))
			}
			b = b[n:]
		default:
			return out
		}
	}
	return out
}

// parseCursorTimestamp parses the time Cursor puts before each prompt,
// "Tuesday, Oct 13, 2026, 12:03 AM (UTC-7)"; zero if it does not parse.
func parseCursorTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	_, rest, ok := strings.Cut(s, ", ")
	if !ok {
		return time.Time{}
	}
	loc := time.UTC
	if i := strings.Index(rest, " (UTC"); i >= 0 {
		off := strings.TrimSuffix(rest[i+len(" (UTC"):], ")")
		rest = rest[:i]
		if off != "" {
			sign := 1
			switch off[0] {
			case '-':
				sign = -1
				off = off[1:]
			case '+':
				off = off[1:]
			}
			hs, ms, _ := strings.Cut(off, ":")
			h, herr := strconv.Atoi(hs)
			m := 0
			if ms != "" {
				m, _ = strconv.Atoi(ms)
			}
			if herr != nil {
				return time.Time{}
			}
			loc = time.FixedZone("", sign*(h*3600+m*60))
		}
	}
	t, err := time.ParseInLocation("Jan 2, 2006, 3:04 PM", rest, loc)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// List implements Reader.
func (c *CursorCLI) List(ctx context.Context, count int, opts Options) (Page, error) {
	if err := checkListCount(count); err != nil {
		return Page{}, err
	}
	cands, err := c.candidates(ctx)
	if err != nil {
		return Page{}, err
	}
	return c.store().list(ctx, cands, count, opts)
}

// Read implements Reader.
func (c *CursorCLI) Read(ctx context.Context, q Query, opts Options) (Page, error) {
	if err := readCLI(SourceCursorCLI, q); err != nil {
		return Page{}, err
	}
	cands, err := c.candidates(ctx)
	if err != nil {
		return Page{}, err
	}
	return c.store().read(ctx, cands, q, opts)
}

// CursorIDE reads the Cursor editor's chats (composers) from its global
// state database, <Dir>/state.vscdb: the cursorDiskKV table holds
// composerData:<composer id> (name, times, and the ordered bubble
// headers) and bubbleId:<composer id>:<bubble id> (one message: type 1
// is the owner's, type 2 the model's). The database is copied with its
// -wal and -shm before it is opened (withSQLiteCopy). The editor's
// per-workspace state databases hold no chats.
type CursorIDE struct {
	// Dir is Cursor's globalStorage directory, under the OS user config
	// dir (~/Library/Application Support/Cursor/User/globalStorage on
	// macOS, ~/.config/Cursor/User/globalStorage on Linux).
	Dir     string
	WakeDir string
	TempDir string
	Window  Window
	Now     func() time.Time
}

// NewCursorIDE returns a reader for this user's Cursor install.
func NewCursorIDE() *CursorIDE {
	dir := ""
	if c, err := os.UserConfigDir(); err == nil {
		dir = filepath.Join(c, "Cursor", "User", "globalStorage")
	}
	return &CursorIDE{Dir: dir, WakeDir: configPath("", "")}
}

// Source implements Reader.
func (c *CursorIDE) Source() Source { return SourceCursorIDE }

func (c *CursorIDE) store() cliStore {
	return cliStore{source: SourceCursorIDE, wakeDir: c.WakeDir, window: c.Window, now: c.Now}
}

// with runs fn on the composers of a copy of the state database, newest
// first.
func (c *CursorIDE) with(ctx context.Context, fn func(cands []cliSession) error) error {
	path := filepath.Join(c.Dir, "state.vscdb")
	err := withSQLiteCopy(path, c.TempDir, func(db *sql.DB) error {
		cands, err := cursorIDECandidates(ctx, db)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return noHistory("Cursor IDE", path)
		}
		return fn(cands)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return noHistory("Cursor IDE", path)
	}
	return err
}

// cursorIDEComposer is the part of composerData the reader uses.
type cursorIDEComposer struct {
	ComposerID    string `json:"composerId"`
	Name          string `json:"name"`
	CreatedAt     int64  `json:"createdAt"`
	LastUpdatedAt int64  `json:"lastUpdatedAt"`
	Headers       []struct {
		BubbleID string `json:"bubbleId"`
		Type     int    `json:"type"`
	} `json:"fullConversationHeadersOnly"`
}

func cursorIDECandidates(ctx context.Context, db *sql.DB) ([]cliSession, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []cliSession
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		var cd cursorIDEComposer
		id := strings.TrimPrefix(key, "composerData:")
		if json.Unmarshal(raw, &cd) != nil || len(cd.Headers) == 0 || !idPattern.MatchString(id) {
			continue
		}
		e := cliSession{id: id, title: cd.Name, updated: msTime(max(cd.LastUpdatedAt, cd.CreatedAt))}
		e.load = func() ([]turn, string, error) { return cursorIDETurns(ctx, db, id, cd, e.updated) }
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].updated.After(out[j].updated) })
	return out, nil
}

// cursorIDETurns reads a composer's bubbles in header order.
func cursorIDETurns(ctx context.Context, db *sql.DB, id string, cd cursorIDEComposer, updated time.Time) ([]turn, string, error) {
	var turns []turn
	for _, h := range cd.Headers {
		var raw []byte
		err := db.QueryRowContext(ctx, `SELECT value FROM cursorDiskKV WHERE key = ?`, "bubbleId:"+id+":"+h.BubbleID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		var b struct {
			Type      int             `json:"type"`
			Text      string          `json:"text"`
			CreatedAt json.RawMessage `json:"createdAt"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		text := strings.TrimSpace(b.Text)
		switch b.Type {
		case 1:
			if text == "" {
				continue
			}
			at := jsonTime(b.CreatedAt)
			if at.IsZero() {
				at = updated
			}
			turns = append(turns, turn{prompt: Message{Role: RoleUser, Text: text, Time: at}})
		case 2:
			if text != "" && len(turns) > 0 {
				turns[len(turns)-1].reply = Message{Role: RoleAssistant, Text: text}
			}
		}
	}
	return turns, "", nil
}

// jsonTime reads an RFC 3339 string or a Unix millisecond number.
func jsonTime(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return parseTime(s)
	}
	var ms int64
	if json.Unmarshal(raw, &ms) == nil {
		return msTime(ms)
	}
	return time.Time{}
}

// List implements Reader.
func (c *CursorIDE) List(ctx context.Context, count int, opts Options) (page Page, err error) {
	if err := checkListCount(count); err != nil {
		return Page{}, err
	}
	err = c.with(ctx, func(cands []cliSession) error {
		page, err = c.store().list(ctx, cands, count, opts)
		return err
	})
	return page, err
}

// Read implements Reader.
func (c *CursorIDE) Read(ctx context.Context, q Query, opts Options) (page Page, err error) {
	if err := readCLI(SourceCursorIDE, q); err != nil {
		return Page{}, err
	}
	err = c.with(ctx, func(cands []cliSession) error {
		page, err = c.store().read(ctx, cands, q, opts)
		return err
	})
	return page, err
}
