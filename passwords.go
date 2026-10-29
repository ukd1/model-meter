package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

const passwordIterations = 600000

func passwordHash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordIterations, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}
func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 100000 || iterations > 2000000 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil || len(salt) != 16 {
		return false
	}
	expected, err := hex.DecodeString(parts[3])
	if err != nil || len(expected) != 32 {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	return err == nil && subtle.ConstantTimeCompare(key, expected) == 1
}
func validUsername(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '@') {
			return false
		}
	}
	return true
}
func normalizeUsername(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func validPassword(s string) bool       { return len(s) >= 12 && len(s) <= 1024 }

// Bootstrap credentials are a normal named administrator account. The generated
// password is printed once for local setup; it is never accepted as a token.
func bootstrapAdmin(db *sql.DB) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE admin=1 AND password_hash<>''`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	username := "admin"
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE username=?`, username).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		username = "admin-" + token()[:8]
	}
	password := token()
	encoded, err := passwordHash(password)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO users(name,username,password_hash,hash,admin,created) VALUES(?,?,?,?,1,?)`, "Administrator", username, encoded, hash(token()), time.Now().UTC().Format(time.RFC3339))
	if err == nil {
		log.Printf("Initial administrator username: %s password: %s", username, password)
	}
	return err
}

func resetPassword(db *sql.DB, username string) (string, error) {
	password := token()
	encoded, err := passwordHash(password)
	if err != nil {
		return "", err
	}
	res, err := db.Exec(`UPDATE users SET password_hash=?,disabled=0 WHERE username=?`, encoded, normalizeUsername(username))
	if err != nil {
		return "", err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if count == 0 {
		return "", fmt.Errorf("user not found")
	}
	return password, nil
}
