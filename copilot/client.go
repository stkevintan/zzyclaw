package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultModel          = "gpt-4o"
	defaultEmbeddingModel = "text-embedding-3-small"
	tokenURL              = "https://api.github.com/copilot_internal/v2/token"
	defaultBaseURL        = "https://api.githubcopilot.com"
	tokenSafeMargin       = 5 * time.Minute
)

// Client is a GitHub Copilot API client that automatically manages
// short-lived Copilot API tokens exchanged from a long-lived GitHub token.
type Client struct {
	githubToken string
	model       string
	httpClient  *http.Client

	mu        sync.Mutex
	apiToken  string
	baseURL   string
	expiresAt time.Time
}

// Option configures the Client.
type Option func(*Client)

func WithModel(model string) Option {
	return func(c *Client) { c.model = model }
}

func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// NewClient creates a new Copilot API client.
// githubToken is the long-lived GitHub personal access token (e.g. from `gh auth token`).
// The client automatically exchanges it for short-lived Copilot API tokens.
func NewClient(githubToken string, opts ...Option) *Client {
	c := &Client{
		githubToken: githubToken,
		model:       defaultModel,
		httpClient:  http.DefaultClient,
		baseURL:     defaultBaseURL,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// copilotTokenResponse is the response from the Copilot token exchange endpoint.
type copilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// resolveToken returns a valid Copilot API token, refreshing if needed.
func (c *Client) resolveToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.apiToken != "" && time.Now().Before(c.expiresAt.Add(-tokenSafeMargin)) {
		return c.apiToken, nil
	}

	slog.Debug("exchanging github token for copilot api token")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("copilot: create token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.96.2")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot: token request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close token response body", "error", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("copilot: read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("copilot: token exchange failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var tokenResp copilotTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("copilot: unmarshal token response: %w", err)
	}

	if tokenResp.Token == "" {
		return "", fmt.Errorf("copilot: token response missing token")
	}

	c.apiToken = tokenResp.Token
	// GitHub returns Unix seconds; handle both seconds and milliseconds.
	if tokenResp.ExpiresAt < 100_000_000_000 {
		c.expiresAt = time.Unix(tokenResp.ExpiresAt, 0)
	} else {
		c.expiresAt = time.UnixMilli(tokenResp.ExpiresAt)
	}

	slog.Debug("copilot api token acquired", "expires_at", c.expiresAt)
	return c.apiToken, nil
}

// Message represents a chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function definition portion of a Tool.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is a function invocation requested by the model.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds the called function name and raw JSON arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Tools          []Tool          `json:"tools,omitempty"`
	ToolChoice     string          `json:"tool_choice,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []choice `json:"choices"`
}

type choice struct {
	Message Message `json:"message"`
}

type responsesRequest struct {
	Model string         `json:"model"`
	Input []Message      `json:"input"`
	Text  *responsesText `json:"text,omitempty"`
}

type responsesText struct {
	Format responseFormat `json:"format"`
}

type responsesResponse struct {
	Output []responsesOutput `json:"output"`
}

type responsesOutput struct {
	Type    string             `json:"type"`
	Content []responsesContent `json:"content"`
}

type responsesContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Chat sends messages to the completions API and returns the text response.
func (c *Client) Chat(ctx context.Context, messages []Message) (string, error) {
	reqBody := chatRequest{
		Model:    c.model,
		Messages: messages,
	}
	return c.do(ctx, reqBody)
}

// ChatResult is the outcome of a ChatWithTools call: either textual content,
// or one or more tool calls the model wants executed (or both).
type ChatResult struct {
	Content   string
	ToolCalls []ToolCall
}

// ChatWithTools sends messages plus tool definitions to the chat completions
// API and returns the assistant's reply, which may contain tool calls. It uses
// the /chat/completions endpoint, which supports OpenAI-style function calling.
func (c *Client) ChatWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResult, error) {
	token, err := c.resolveToken(ctx)
	if err != nil {
		return nil, err
	}

	reqBody := chatRequest{
		Model:    c.model,
		Messages: messages,
		Tools:    tools,
	}

	msg, err := c.chatCompletionMessage(ctx, token, reqBody)
	if err != nil {
		return nil, err
	}
	return &ChatResult{Content: msg.Content, ToolCalls: msg.ToolCalls}, nil
}

// chatCompletionMessage posts to /chat/completions and returns the raw
// assistant message, preserving any tool calls.
func (c *Client) chatCompletionMessage(ctx context.Context, token string, reqBody chatRequest) (*Message, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("copilot: marshal request: %w", err)
	}

	endpoint := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("copilot: create request: %w", err)
	}
	c.setHeaders(req, token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot: request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close chat response body", "error", closeErr)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("copilot: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot: status %d: %s", resp.StatusCode, respBody)
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("copilot: unmarshal response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("copilot: empty response")
	}

	return &chatResp.Choices[0].Message, nil
}

// Parse sends a system prompt and user content to the completions API with
// JSON mode enabled, and unmarshals the response into type T.
func Parse[T any](ctx context.Context, c *Client, systemPrompt, userContent string) (*T, error) {
	reqBody := chatRequest{
		Model: c.model,
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		ResponseFormat: &responseFormat{Type: "json_object"},
	}

	respMsg, err := c.do(ctx, reqBody)
	if err != nil {
		return nil, err
	}

	// Pre-process: coerce types to be flexible with LLM output.
	// Convert float64 → string (for string fields) and string-encoded numbers → float64 (for int fields).
	// json.Unmarshal will then handle the correct mapping.
	var raw map[string]any
	if err := json.Unmarshal([]byte(respMsg), &raw); err != nil {
		return nil, fmt.Errorf("copilot: unmarshal result: %w", err)
	}

	// First pass: try unmarshalling as-is to detect type mismatches
	firstTry, _ := json.Marshal(raw)
	var result T
	if err := json.Unmarshal(firstTry, &result); err != nil {
		// Apply type coercion and retry
		for k, v := range raw {
			switch val := v.(type) {
			case float64:
				// Keep as float64 but also provide string form
				raw[k] = fmt.Sprintf("%v", val)
			case string:
				// Try converting string to number for numeric fields
				if n, err := strconv.ParseFloat(val, 64); err == nil {
					raw[k] = n
				}
			}
		}
		normalized, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("copilot: re-marshal result: %w", err)
		}

		// Second attempt — if this also fails, try the opposite coercion
		if err := json.Unmarshal(normalized, &result); err != nil {
			return nil, fmt.Errorf("copilot: unmarshal result: %w", err)
		}
	}
	return &result, nil
}

func (c *Client) do(ctx context.Context, reqBody chatRequest) (string, error) {
	token, err := c.resolveToken(ctx)
	if err != nil {
		return "", err
	}

	result, err := c.doResponses(ctx, token, reqBody)
	if err != nil && strings.Contains(err.Error(), "unsupported_api_for_model") {
		slog.Info("model not supported via /responses, falling back to /chat/completions", "model", reqBody.Model)
		return c.doChatCompletions(ctx, token, reqBody)
	}
	return result, err
}

func (c *Client) setHeaders(req *http.Request, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Editor-Version", "vscode/1.96.2")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")
}

func (c *Client) doChatCompletions(ctx context.Context, token string, reqBody chatRequest) (string, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("copilot: marshal request: %w", err)
	}

	endpoint := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("copilot: create request: %w", err)
	}
	c.setHeaders(req, token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot: request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close chat response body", "error", closeErr)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("copilot: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("copilot: status %d: %s", resp.StatusCode, respBody)
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("copilot: unmarshal response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("copilot: empty response")
	}

	return chatResp.Choices[0].Message.Content, nil
}

func (c *Client) doResponses(ctx context.Context, token string, reqBody chatRequest) (string, error) {
	rr := responsesRequest{
		Model: reqBody.Model,
		Input: reqBody.Messages,
	}
	if reqBody.ResponseFormat != nil {
		rr.Text = &responsesText{Format: *reqBody.ResponseFormat}
	}

	body, err := json.Marshal(rr)
	if err != nil {
		return "", fmt.Errorf("copilot: marshal request: %w", err)
	}

	endpoint := c.baseURL + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("copilot: create request: %w", err)
	}
	c.setHeaders(req, token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot: request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close responses body", "error", closeErr)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("copilot: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("copilot: status %d: %s", resp.StatusCode, respBody)
	}

	var rResp responsesResponse
	if err := json.Unmarshal(respBody, &rResp); err != nil {
		return "", fmt.Errorf("copilot: unmarshal response: %w", err)
	}

	for _, out := range rResp.Output {
		if out.Type != "message" {
			continue
		}
		for _, c := range out.Content {
			if c.Type == "output_text" {
				return c.Text, nil
			}
		}
	}

	return "", fmt.Errorf("copilot: empty response")
}

// embeddingsRequest is the request body for the /embeddings endpoint.
type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embeddingsResponse is the OpenAI-style response from /embeddings.
type embeddingsResponse struct {
	Data []embeddingDatum `json:"data"`
}

type embeddingDatum struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// Embeddings returns one vector per input string, in input order, using the
// given embedding model (empty uses the default). It targets the same base URL
// as chat; the /embeddings endpoint additionally requires the
// Copilot-Integration-Id header.
func (c *Client) Embeddings(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if model == "" {
		model = defaultEmbeddingModel
	}

	token, err := c.resolveToken(ctx)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(embeddingsRequest{Model: model, Input: inputs})
	if err != nil {
		return nil, fmt.Errorf("copilot: marshal embeddings request: %w", err)
	}

	endpoint := c.baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("copilot: create embeddings request: %w", err)
	}
	c.setHeaders(req, token)
	// The /embeddings endpoint rejects requests without an integration id
	// (manifests as a connection reset), unlike /chat/completions.
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot: embeddings request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close embeddings body", "error", closeErr)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("copilot: read embeddings response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot: embeddings status %d: %s", resp.StatusCode, respBody)
	}

	var er embeddingsResponse
	if err := json.Unmarshal(respBody, &er); err != nil {
		return nil, fmt.Errorf("copilot: unmarshal embeddings response: %w", err)
	}
	if len(er.Data) != len(inputs) {
		return nil, fmt.Errorf("copilot: embeddings returned %d vectors for %d inputs", len(er.Data), len(inputs))
	}

	// The API may return data out of order; place each vector by its index.
	out := make([][]float32, len(inputs))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("copilot: embeddings response index %d out of range", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if len(v) == 0 {
			return nil, fmt.Errorf("copilot: embeddings missing vector for input %d", i)
		}
	}
	return out, nil
}
