package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Existing accounting rows keep their IDs and key references. SQLite requires
// rebuilding a table to remove NOT NULL; the transaction also preserves indexes
// and triggers so a failed migration leaves the original table intact.
func migrateAnonymousRequests(db *sql.DB) error {
	var required bool
	if err := db.QueryRow(`SELECT "notnull" FROM pragma_table_info('requests') WHERE name='key_id'`).Scan(&required); err != nil {
		return err
	}
	if !required {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT sql FROM sqlite_master WHERE tbl_name='requests' AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY type,name`)
	if err != nil {
		return err
	}
	var schema []string
	for rows.Next() {
		var statement string
		if err = rows.Scan(&statement); err != nil {
			rows.Close()
			return err
		}
		schema = append(schema, statement)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var legacyColumns int
	if err = tx.QueryRow("SELECT count(*) FROM pragma_table_info('requests') WHERE name='legacy_request_id'").Scan(&legacyColumns); err != nil {
		return err
	}
	extraDefinition, extraNames := "", ""
	if legacyColumns > 0 {
		extraDefinition = ",legacy_request_id TEXT UNIQUE,legacy_reference_usd REAL,legacy_status TEXT"
		extraNames = ",legacy_request_id,legacy_reference_usd,legacy_status"
	}
	_, err = tx.Exec("CREATE TABLE requests_nullable(id INTEGER PRIMARY KEY,key_id INTEGER REFERENCES keys(id),time TEXT NOT NULL,model TEXT NOT NULL,method TEXT NOT NULL,path TEXT NOT NULL,source TEXT NOT NULL,status INTEGER NOT NULL,duration_ms INTEGER NOT NULL,input_tokens INTEGER,output_tokens INTEGER,cached_tokens INTEGER,outcome TEXT NOT NULL" + extraDefinition + ");" +
		" INSERT INTO requests_nullable(id,key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,cached_tokens,outcome" + extraNames + ") SELECT id,key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,cached_tokens,outcome" + extraNames + " FROM requests;" +
		" DROP TABLE requests; ALTER TABLE requests_nullable RENAME TO requests;")
	if err != nil {
		return err
	}
	for _, statement := range schema {
		if _, err = tx.Exec(statement); err != nil {
			return err
		}
	}
	var violations int
	if err = tx.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check('requests')`).Scan(&violations); err != nil {
		return err
	}
	if violations != 0 {
		return fmt.Errorf("accounting migration found %d invalid key references", violations)
	}
	return tx.Commit()
}

func (a *App) loadSettings() error {
	a.settingsOnce.Do(func() {
		var allowed bool
		err := a.db.QueryRow(`SELECT value FROM settings WHERE name='allow_anonymous'`).Scan(&allowed)
		if err != nil && err != sql.ErrNoRows {
			a.settingsErr = err
			return
		}
		a.allowAnonymous.Store(allowed)
	})
	return a.settingsErr
}

// Only called after administrator authentication and normal dashboard CSRF
// checks. The cached flag keeps anonymous proxy requests off the settings DB.
func (a *App) proxySettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var input struct {
			AllowAnonymous *bool `json:"allow_anonymous"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || input.AllowAnonymous == nil || decoder.Decode(new(any)) != io.EOF {
			fail(w, 400, "allow_anonymous must be a boolean")
			return
		}
		// Serialize writes through the commit and atomic store to keep the cache
		// consistent with the last persisted update under concurrent admins.
		a.settingsMu.Lock()
		_, err := a.db.Exec(`INSERT INTO settings(name,value) VALUES('allow_anonymous',?) ON CONFLICT(name) DO UPDATE SET value=excluded.value`, *input.AllowAnonymous)
		if err == nil {
			a.allowAnonymous.Store(*input.AllowAnonymous)
		}
		a.settingsMu.Unlock()
		if err != nil {
			fail(w, 500, "settings update failed")
			return
		}
		a.live.notify(nil)
	}
	reply(w, 200, map[string]bool{"allow_anonymous": a.allowAnonymous.Load()})
}
