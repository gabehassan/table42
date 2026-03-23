package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const resyCaptchaSiteKey = "6Lfw-dIZAAAAAESRBH4JwdgfTXj5LlS1ewlvvCYe"
const resyCaptchaPageURL = "https://resy.com"

// solveCaptcha solves reCAPTCHA v2 via CAPSolver (5-15s avg, AI-based).
// Returns the gRecaptchaResponse token (valid ~120 seconds).
func solveCaptcha(apiKey string) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("RESY_CAPSOLVER_KEY not set")
	}

	start := time.Now()
	logf("Solving reCAPTCHA v2 via CAPSolver...")

	taskReq := map[string]any{
		"clientKey": apiKey,
		"task": map[string]any{
			"type":       "ReCaptchaV2TaskProxyLess",
			"websiteURL": resyCaptchaPageURL,
			"websiteKey": resyCaptchaSiteKey,
		},
	}
	taskBody, _ := json.Marshal(taskReq)

	resp, err := http.Post("https://api.capsolver.com/createTask", "application/json", bytes.NewReader(taskBody))
	if err != nil {
		return "", fmt.Errorf("capsolver createTask: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var createResp struct {
		ErrorID   int    `json:"errorId"`
		ErrorDesc string `json:"errorDescription"`
		TaskID    string `json:"taskId"`
	}
	json.Unmarshal(respBody, &createResp)

	if createResp.ErrorID != 0 {
		return "", fmt.Errorf("capsolver error: %s", createResp.ErrorDesc)
	}
	if createResp.TaskID == "" {
		return "", fmt.Errorf("capsolver no taskId: %s", respBody)
	}

	// Poll for result
	pollReq := map[string]any{
		"clientKey": apiKey,
		"taskId":    createResp.TaskID,
	}
	pollBody, _ := json.Marshal(pollReq)

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)

		resp, err := http.Post("https://api.capsolver.com/getTaskResult", "application/json", bytes.NewReader(pollBody))
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result struct {
			ErrorID  int    `json:"errorId"`
			ErrorDesc string `json:"errorDescription"`
			Status   string `json:"status"`
			Solution struct {
				GRecaptchaResponse string `json:"gRecaptchaResponse"`
			} `json:"solution"`
		}
		json.Unmarshal(body, &result)

		if result.ErrorID != 0 {
			return "", fmt.Errorf("capsolver poll error: %s", result.ErrorDesc)
		}

		if result.Status == "ready" {
			token := result.Solution.GRecaptchaResponse
			if token == "" {
				return "", fmt.Errorf("capsolver returned empty token")
			}
			logf("CAPSolver solved in %v", time.Since(start).Round(time.Millisecond))
			return token, nil
		}
	}

	return "", fmt.Errorf("capsolver timeout (90s)")
}

// preSolveCaptcha solves captcha ahead of drop time and stores it in cfg.
func preSolveCaptcha(cfg *Config) {
	if cfg.CapSolverKey == "" {
		return
	}

	start := time.Now()
	token, err := solveCaptcha(cfg.CapSolverKey)
	if err != nil {
		logf("Warning: captcha pre-solve failed: %v", err)
		logf("Will attempt booking without captcha token (works on most venues)")
		notifyWebhook(cfg.WebhookURL,
			"Captcha pre-solve failed",
			fmt.Sprintf("**Error:** %v\nWill try without captcha", err),
			false)
		return
	}

	cfg.CaptchaToken = token
	elapsed := time.Since(start).Round(time.Millisecond)
	logf("Captcha token ready in %v (valid ~120s)", elapsed)
	if tlog != nil {
		tlog.record("captcha-solved", 0, len(token), fmt.Sprintf("took=%v", elapsed))
	}
}
