package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// STTService 定義語音轉錄為文字的介面。
type STTService interface {
	STT(ctx context.Context, filePath string) (string, error)
}

// Summarizer 定義 LLM 摘要生成的介面（含一次性與串流）。
type Summarizer interface {
	Summarize(ctx context.Context, text string, prompt string) (string, error)
	SummarizeStream(ctx context.Context, text string, prompt string, onChunk func(chunk string)) error
}

// AIService 組合介面（保留相容性，或作為聯合介面使用）。
type AIService interface {
	STTService
	Summarizer
}

// --- Mock 實作 ---

// MockAIService 模擬 AI 服務，用於開發測試環境。
// 模擬真實的延遲與串流行為，確保前後端整合測試的穩定性。
type MockAIService struct{}

// STT 模擬語音轉錄，隨機延遲 2~4 秒後返回固定文字。
// 它會先檢查檔案是否存在，以確保 Worker 傳入的路徑是正確的。
func (m *MockAIService) STT(ctx context.Context, filePath string) (string, error) {
	if filePath == "" {
		return "", fmt.Errorf("mock stt: file path is empty")
	}
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return "", fmt.Errorf("mock stt: file not found at %s", filePath)
	}

	select {
	case <-time.After(time.Duration(2+rand.Intn(3)) * time.Second):
		return "這是一段模擬的語音轉錄內容。內容包含了一些關於系統架構與設計模式的討論。", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Summarize 模擬一次性摘要生成，會檢查輸入文字是否為空。
func (m *MockAIService) Summarize(ctx context.Context, text string, prompt string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("mock llm: input text is empty")
	}
	select {
	case <-time.After(time.Duration(2+rand.Intn(2)) * time.Second):
		return "摘要：討論了系統的微服務架構，包含 API Gateway、RabbitMQ 與 Worker 的協作模式。", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// SummarizeStream 模擬 LLM 串流摘要，會檢查輸入文字是否為空。
func (m *MockAIService) SummarizeStream(ctx context.Context, text string, prompt string, onChunk func(chunk string)) error {
	if text == "" {
		return fmt.Errorf("mock llm stream: input text is empty")
	}
	chunks := []string{
		"摘要：",
		"本段錄音討論了",
		"系統的微服務架構，",
		"包含 API Gateway、",
		"RabbitMQ 與 Worker 的",
		"協作模式。",
		"\n\n重點包括：\n",
		"一、非同步任務處理；\n",
		"二、串流上傳設計；\n",
		"三、原子狀態管理。",
	}
	for _, chunk := range chunks {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(200+rand.Intn(300)) * time.Millisecond):
			onChunk(chunk)
		}
	}
	return nil
}

// --- OpenAI 實作 ---

// StandardAIProvider 提供符合 OpenAI 規範的 STT 與 LLM 服務。
// 它支援分離的 STT 與 LLM 配置，以便混用不同的提供商（如 OpenAI + 自建服務）。
type StandardAIProvider struct {
	STTApiKey string
	STTURL    string
	STTModel  string
	LLMApiKey string
	LLMURL    string
	LLMModel  string
}

// STT 呼叫 OpenAI 規範的語音轉錄 API。
// 使用 multipart/form-data 格式上傳音檔。
func (o *StandardAIProvider) STT(ctx context.Context, filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", err
	}
	_ = writer.WriteField("model", o.STTModel)
	writer.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", o.STTURL, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+o.STTApiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openai stt failed: %s", string(b))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Text, nil
}

// Summarize 呼叫 OpenAI 規範的 ChatCompletion API 一次性生成摘要。
func (o *StandardAIProvider) Summarize(ctx context.Context, text string, userPrompt string) (string, error) {
	if userPrompt == "" {
		userPrompt = "請摘要以下錄音文字內容："
	}

	payload := map[string]interface{}{
		"model": o.LLMModel,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful assistant that summarizes audio transcripts."},
			{"role": "user", "content": fmt.Sprintf("%s\n\n%s", userPrompt, text)},
		},
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", o.LLMURL, bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.LLMApiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openai llm failed: %s", string(b))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Choices) > 0 {
		return result.Choices[0].Message.Content, nil
	}
	return "", fmt.Errorf("no summary generated")
}

// SummarizeStream 呼叫 OpenAI 規範的 ChatCompletion API（stream=true），
// 逐行解析 SSE 回應並透過 onChunk callback 即時回傳摘要片段。
// Worker 在收到每個 chunk 後同步發布至 Redis Pub/Sub。
func (o *StandardAIProvider) SummarizeStream(ctx context.Context, text string, userPrompt string, onChunk func(chunk string)) error {
	if userPrompt == "" {
		userPrompt = "請摘要以下錄音文字內容："
	}

	// 建立payload
	payload := map[string]interface{}{
		"model": o.LLMModel,
		// 串流設定
		"stream": true,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful assistant that summarizes audio transcripts."},
			{"role": "user", "content": fmt.Sprintf("%s\n\n%s", userPrompt, text)},
		},
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", o.LLMURL, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.LLMApiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("openai stream failed: %s", string(b))
	}

	// 開始解析串流回應
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		// OpenAI SSE 格式約定：每行以 "data: " 開頭
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		// 串流結束標誌
		if data == "[DONE]" {
			break
		}

		// 定義一個臨時struct，專門用來解析當下這一行數據。
		// 結構對應 OpenAI 返回的 JSON 格式：choices[0].delta.content
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}

		// 將 "data: " 後面的 JSON 字串解析進臨時結構中
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 如果解析失敗，跳過這行（有些中間行可能是空的或格式不合）
		}

		// 如果這一片數據中有內容，就透過 callback (onChunk) 丟出去
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			onChunk(chunk.Choices[0].Delta.Content)
		}
	}
	return nil
}
