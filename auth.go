package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type StoredToken struct {
	AuthToken       string    `json:"auth_token"`
	PaymentMethodID int       `json:"payment_method_id"`
	FirstName       string    `json:"first_name"`
	LastName        string    `json:"last_name"`
	ExpiresAt       time.Time `json:"expires_at"`
	Email           string    `json:"email"`
}

var tokenStorePath = filepath.Join(os.Getenv("HOME"), ".noresi", "resy_tokens.json")

func loadTokenStore() map[string]StoredToken {
	data, err := os.ReadFile(tokenStorePath)
	if err != nil {
		return make(map[string]StoredToken)
	}
	var store map[string]StoredToken
	if json.Unmarshal(data, &store) != nil {
		return make(map[string]StoredToken)
	}
	return store
}

func saveTokenStore(store map[string]StoredToken) {
	dir := filepath.Dir(tokenStorePath)
	os.MkdirAll(dir, 0700)
	data, _ := json.MarshalIndent(store, "", "  ")
	os.WriteFile(tokenStorePath, data, 0600)
}

// getAuthToken returns (authToken, paymentMethodID) — tries cache first, then login.
func getAuthToken(email, password string) (string, int, error) {
	store := loadTokenStore()

	// Check cache — 5 minute safety buffer
	if tok, ok := store[email]; ok {
		if time.Now().Before(tok.ExpiresAt.Add(-5 * time.Minute)) {
			logf("Using cached auth token for %s (expires %s)", email, tok.ExpiresAt.Format(time.RFC3339))
			return tok.AuthToken, tok.PaymentMethodID, nil
		}
		logf("Cached token expired for %s, re-authenticating...", email)
	}

	// Login with email+password
	authToken, paymentID, firstName, lastName, err := loginResy(email, password)
	if err != nil {
		return "", 0, err
	}

	// Cache the token — Resy tokens don't have a documented expiry,
	// use 24 hours as a conservative default
	store[email] = StoredToken{
		AuthToken:       authToken,
		PaymentMethodID: paymentID,
		FirstName:       firstName,
		LastName:        lastName,
		ExpiresAt:       time.Now().Add(24 * time.Hour),
		Email:           email,
	}
	saveTokenStore(store)

	logf("Authenticated as %s %s (%s)", firstName, lastName, email)
	return authToken, paymentID, nil
}

// loginResy authenticates via email+password → returns (authToken, paymentMethodID, firstName, lastName, error)
func loginResy(email, password string) (string, int, string, string, error) {
	form := url.Values{}
	form.Set("email", email)
	form.Set("password", password)
	body := []byte(form.Encode())

	req, _ := http.NewRequest("POST", "https://api.resy.com/3/auth/password", bytes.NewReader(body))
	req.Header.Set("Authorization", resyAPIKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", "https://resy.com")
	req.Header.Set("Referer", "https://resy.com/")
	req.Header.Set("X-Origin", "https://resy.com")
	req.Header.Set("Cache-Control", "no-cache")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, "", "", fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", 0, "", "", fmt.Errorf("login failed (HTTP %d): %s", resp.StatusCode, data)
	}

	// Byte-scan for the fields we need — handles both compact and pretty-printed JSON
	token := extractJSONValue(data, []byte(`"token":`))
	if token == "" {
		return "", 0, "", "", fmt.Errorf("no auth token in login response: %s", data)
	}

	firstName := extractJSONValue(data, []byte(`"first_name":`))
	lastName := extractJSONValue(data, []byte(`"last_name":`))
	paymentID := extractJSONNumber(data, []byte(`"payment_method_id":`))

	return token, paymentID, firstName, lastName, nil
}

// getPaymentMethodID fetches the user's payment methods from the account profile.
// Tries GET /2/user which returns payment_method_id and payment_methods[].
// Required for venues with deposits — payment_id=0 works for no-deposit venues.
func getPaymentMethodID(authToken string) (int, error) {
	req, _ := http.NewRequest("GET", "https://api.resy.com/2/user", nil)
	req.Header.Set("Authorization", resyAPIKey)
	req.Header.Set("X-Resy-Auth-Token", authToken)
	req.Header.Set("X-Resy-Universal-Auth", authToken)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", "https://resy.com")
	req.Header.Set("Referer", "https://resy.com/")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("payment method request failed: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("payment method fetch failed (HTTP %d): %s", resp.StatusCode, data)
	}

	// Try payment_method_id first (top-level field on user profile)
	id := extractJSONNumber(data, []byte(`"payment_method_id":`))
	if id != 0 {
		return id, nil
	}

	// Fallback: scan for first "id" inside payment_methods array
	pmIdx := bytes.Index(data, []byte(`"payment_methods"`))
	if pmIdx >= 0 {
		id = extractJSONNumber(data[pmIdx:], []byte(`"id":`))
		if id != 0 {
			return id, nil
		}
	}

	return 0, fmt.Errorf("no payment method found — add a credit card at resy.com/account")
}
