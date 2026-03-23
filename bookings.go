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

type Booking struct {
	VenueID       int       `json:"venue_id"`
	VenueName     string    `json:"venue_name"`
	ReservationID string    `json:"reservation_id"`
	ResyToken     string    `json:"resy_token"`
	DateTime      string    `json:"date_time"`
	PartySize     int       `json:"party_size"`
	TableType     string    `json:"table_type"`
	BookedAt      time.Time `json:"booked_at"`
}

var bookingsPath = filepath.Join(os.Getenv("HOME"), ".noresi", "resy_bookings.json")

func loadBookings() []Booking {
	data, err := os.ReadFile(bookingsPath)
	if err != nil {
		return nil
	}
	var bookings []Booking
	json.Unmarshal(data, &bookings)
	return bookings
}

func saveBooking(b Booking) {
	dir := filepath.Dir(bookingsPath)
	os.MkdirAll(dir, 0700)
	bookings := loadBookings()
	bookings = append(bookings, b)
	data, _ := json.MarshalIndent(bookings, "", "  ")
	os.WriteFile(bookingsPath, data, 0600)
}

func handleCancel(args []string) {
	if len(args) == 0 {
		// List all bookings
		bookings := loadBookings()
		if len(bookings) == 0 {
			fmt.Println("No bookings found.")
			return
		}
		for i, b := range bookings {
			fmt.Printf("[%d] %s — %s, party of %d (%s) — booked %s\n",
				i, b.VenueName, b.DateTime, b.PartySize, b.TableType,
				b.BookedAt.Format("2006-01-02 15:04"))
		}
		return
	}

	target := args[0]

	bookings := loadBookings()
	if len(bookings) == 0 {
		fatal("No bookings to cancel.")
	}

	if target == "all" {
		var failed []Booking
		for _, b := range bookings {
			if err := cancelReservation(b.ResyToken); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to cancel %s (%s): %v\n", b.VenueName, b.ReservationID, err)
				failed = append(failed, b) // keep failed ones in the file
			} else {
				fmt.Printf("Cancelled: %s — %s\n", b.VenueName, b.DateTime)
			}
		}
		data, _ := json.MarshalIndent(failed, "", "  ")
		if len(failed) == 0 {
			data = []byte("[]")
		}
		os.WriteFile(bookingsPath, data, 0600)
		return
	}

	// Cancel by reservation ID
	var remaining []Booking
	cancelled := false
	for _, b := range bookings {
		if b.ReservationID == target {
			if err := cancelReservation(b.ResyToken); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to cancel %s: %v\n", target, err)
				remaining = append(remaining, b)
			} else {
				fmt.Printf("Cancelled: %s — %s\n", b.VenueName, b.DateTime)
				cancelled = true
			}
		} else {
			remaining = append(remaining, b)
		}
	}

	if !cancelled {
		fatal("Reservation %s not found. Run 'table42 cancel' to list bookings.", target)
	}

	data, _ := json.MarshalIndent(remaining, "", "  ")
	os.WriteFile(bookingsPath, data, 0600)
}

func cancelReservation(resyToken string) error {
	form := url.Values{}
	form.Set("resy_token", resyToken)
	body := []byte(form.Encode())

	// Get auth token — try package-level var first (set during main flow),
	// then env var, then email+password login
	token := authHeader
	if token == "" {
		token = os.Getenv("RESY_AUTH_TOKEN")
	}
	if token == "" {
		email := os.Getenv("RESY_EMAIL")
		password := os.Getenv("RESY_PASSWORD")
		if email != "" && password != "" {
			var err error
			token, _, err = getAuthToken(email, password)
			if err != nil {
				return fmt.Errorf("auth failed for cancel: %w", err)
			}
		}
	}

	if token == "" {
		return fmt.Errorf("no auth credentials — set RESY_AUTH_TOKEN in .env")
	}

	req, _ := http.NewRequest("POST", "https://api.resy.com/3/cancel", bytes.NewReader(body))
	req.Header.Set("Authorization", resyAPIKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", "https://resy.com")
	req.Header.Set("Referer", "https://resy.com/")
	req.Header.Set("X-Origin", "https://resy.com")
	if token != "" {
		req.Header.Set("X-Resy-Auth-Token", token)
		req.Header.Set("X-Resy-Universal-Auth", token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cancel request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel failed (HTTP %d): %s", resp.StatusCode, data)
	}

	return nil
}
