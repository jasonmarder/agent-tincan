package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OpenCode reads local OpenCode sessions from its SQLite database,
// opencode.db in its data dir: the session table (id, parent_id,
// directory, title, times in Unix ms), message rows whose data JSON holds
// the role, and part rows whose data JSON holds a text part's text. The
// owner's prompt is a user message's text parts that OpenCode did not add
// itself (synthetic or ignored); the reply is the last assistant text
// before the next prompt. The database is copied with its -wal and -shm
// before it is opened (withSQLiteCopy).
//
// Sessions with a parent are subagent tasks, not the owner's, and count
// as automated like wake runs (ids in a <name>.wake-sessions list in
// WakeDir, or a directory in a wake's workdir or home). Sessions in
// ScratchDir are never reported.
type OpenCode struct {
	// DB is the database path: $OPENCODE_DB (relative to the data dir
	// unless absolute), else opencode.db in $XDG_DATA_HOME/opencode or
	// ~/.local/share/opencode.
	DB         string
	WakeDir    string
	ScratchDir string
	TempDir    string
	Window     Window
	Now        func() time.Time
}

// NewOpenCode returns a reader for this user's OpenCode database.
func NewOpenCode() *OpenCode {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		if h, err := os.UserHomeDir(); err == nil {
			data = filepath.Join(h, ".local", "share")
		}
	}
	dir := filepath.Join(data, "opencode")
	db := filepath.Join(dir, "opencode.db")
	if o := os.Getenv("OPENCODE_DB"); o != "" {
		db = o
		if !filepath.IsAbs(o) {
			db = filepath.Join(dir, o)
		}
	}
	return &OpenCode{DB: db, WakeDir: configPath("", ""), ScratchDir: DefaultScratchDir()}
}

// Source implements Reader.
func (o *OpenCode) Source() Source { return SourceOpenCode }

func (o *OpenCode) store() cliStore {
	return cliStore{source: SourceOpenCode, wakeDir: o.WakeDir, scratchDir: o.ScratchDir, window: o.Window, now: o.Now}
}

// with runs fn on the sessions of a copy of the database, newest first.
func (o *OpenCode) with(ctx context.Context, fn func(cands []cliSession) error) error {
	err := withSQLiteCopy(o.DB, o.TempDir, func(db *sql.DB) error {
		cands, err := openCodeCandidates(ctx, db)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return noHistory("OpenCode", o.DB)
		}
		return fn(cands)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return noHistory("OpenCode", o.DB)
	}
	return err
}

func openCodeCandidates(ctx context.Context, db *sql.DB) ([]cliSession, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, parent_id, directory, title, time_created, time_updated FROM session ORDER BY time_updated DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []cliSession
	for rows.Next() {
		var id, dir, title string
		var parent sql.NullString
		var created, updated int64
		if err := rows.Scan(&id, &parent, &dir, &title, &created, &updated); err != nil {
			return nil, err
		}
		if !idPattern.MatchString(id) {
			continue
		}
		e := cliSession{id: id, title: title, cwd: dir, updated: msTime(max(updated, created)), automated: parent.Valid && parent.String != ""}
		e.load = func() ([]turn, string, error) { return openCodeTurns(ctx, db, id, e.updated) }
		out = append(out, e)
	}
	return out, rows.Err()
}

// openCodeTurns reads one session's messages and their text parts in
// order.
func openCodeTurns(ctx context.Context, db *sql.DB, id string, updated time.Time) ([]turn, string, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.time_created, m.data, p.data FROM message m LEFT JOIN part p ON p.message_id = m.id
		WHERE m.session_id = ? ORDER BY m.time_created, m.id, p.id`, id)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	type message struct {
		id, role string
		at       time.Time
		texts    []string
	}
	var msgs []*message
	for rows.Next() {
		var mid string
		var created int64
		var mdata []byte
		var pdata sql.NullString
		if err := rows.Scan(&mid, &created, &mdata, &pdata); err != nil {
			return nil, "", err
		}
		if len(msgs) == 0 || msgs[len(msgs)-1].id != mid {
			var info struct {
				Role string `json:"role"`
			}
			_ = json.Unmarshal(mdata, &info)
			msgs = append(msgs, &message{id: mid, role: info.Role, at: msTime(created)})
		}
		if !pdata.Valid {
			continue
		}
		var p struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Synthetic bool   `json:"synthetic"`
			Ignored   bool   `json:"ignored"`
		}
		if json.Unmarshal([]byte(pdata.String), &p) != nil || p.Type != "text" || p.Synthetic || p.Ignored {
			continue
		}
		cur := msgs[len(msgs)-1]
		cur.texts = append(cur.texts, p.Text)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var turns []turn
	for _, m := range msgs {
		text := strings.TrimSpace(strings.Join(m.texts, "\n"))
		if text == "" {
			continue
		}
		switch m.role {
		case "user":
			at := m.at
			if at.IsZero() {
				at = updated
			}
			turns = append(turns, turn{prompt: Message{Role: RoleUser, Text: text, Time: at}})
		case "assistant":
			if len(turns) > 0 {
				turns[len(turns)-1].reply = Message{Role: RoleAssistant, Text: text}
			}
		}
	}
	return turns, "", nil
}

// List implements Reader.
func (o *OpenCode) List(ctx context.Context, count int, opts Options) (page Page, err error) {
	if err := checkListCount(count); err != nil {
		return Page{}, err
	}
	err = o.with(ctx, func(cands []cliSession) error {
		page, err = o.store().list(ctx, cands, count, opts)
		return err
	})
	return page, err
}

// Read implements Reader.
func (o *OpenCode) Read(ctx context.Context, q Query, opts Options) (page Page, err error) {
	if err := readCLI(SourceOpenCode, q); err != nil {
		return Page{}, err
	}
	err = o.with(ctx, func(cands []cliSession) error {
		page, err = o.store().read(ctx, cands, q, opts)
		return err
	})
	return page, err
}
