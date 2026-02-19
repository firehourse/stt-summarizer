package audio

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Chunk 代表切割後的音檔分片，Index 用於合併時的排序依據。
type Chunk struct {
	Index    int
	FilePath string
}

const (
	// MaxFileSizeNoSplit 定義不進行切割的最大檔案大小。
	// 設為 1MB 以符合本地開發及低延遲處理需求。
	MaxFileSizeNoSplit = 1 * 1024 * 1024
	// BytesPerSecond16kMono 為 16kHz Mono 16-bit WAV 的位元率 (32,000 bytes/s)。
	BytesPerSecond16kMono = 32000
)

// SplitAudio 將音檔切割為符合 STT 模型限制的分片。
//
// 策略：
//   - 小於 MaxFileSizeNoSplit 的檔案直接轉換格式，不切割
//   - VAD 優先：在硬性上限 (maxChunkDuration) 之前尋找最晚的靜音點
//   - Overlap Fallback：無合適靜音點時執行硬切，銜接處加入 1.5s 重疊防止斷詞
//   - 格式標準化：所有分片統一轉換為 16kHz Mono 16-bit WAV (保證大小)
func SplitAudio(inputPath string, maxChunkDuration float64) ([]Chunk, error) {
	tempDir := filepath.Join(filepath.Dir(inputPath), "chunks")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, err
	}

	duration, err := getDuration(inputPath)
	if err != nil {
		return nil, err
	}

	// 根據時長預估輸出大小，確保轉換後的單一 WAV 檔案不超過 MaxFileSizeNoSplit
	if duration*float64(BytesPerSecond16kMono) < float64(MaxFileSizeNoSplit) {
		outputPath := filepath.Join(tempDir, "chunk_0.wav")
		cmd := exec.Command("ffmpeg", "-y", "-i", inputPath, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", outputPath)
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("failed to convert audio: %v", err)
		}
		return []Chunk{{Index: 0, FilePath: outputPath}}, nil
	}

	silences, err := getSilencePoints(inputPath)
	if err != nil {
		// VAD 偵測失敗時退化為固定時長切割
		silences = []float64{}
	}

	const overlapDuration = 1.5 // 無靜音點時的重疊秒數，防止硬切斷詞

	var chunks []Chunk
	index := 0
	start := 0.0

	for start < duration {
		targetEnd := start + maxChunkDuration
		if targetEnd > duration {
			targetEnd = duration
		}

		// 實作硬性上限搜尋：在不超過 targetEnd 的前提下，尋找最晚的靜音點 (確保單一分片 < 1MB)
		actualEnd := targetEnd
		usedSilence := false
		if targetEnd < duration {
			bestSilence := -1.0

			// 僅搜尋 (start, targetEnd] 範圍內的靜音點，確保不超標
			for _, s := range silences {
				if s > start && s <= targetEnd {
					// 取範圍內最後一個（最靠近上限）的靜音點，極大化分片效率
					if s > bestSilence {
						bestSilence = s
					}
				}
			}

			// 啟發式規則：僅在靜音點位於目標點前的 10s 內才採用
			// 若靜音點太早，則直接執行硬切（透過 1.5s Overlap 補償語義中斷）
			if bestSilence != -1.0 && (targetEnd-bestSilence) < 10.0 {
				actualEnd = bestSilence
				usedSilence = true
			}
		}

		// 避免尾端產生過短分片（<5s 直接合併至當前分片）
		if duration-actualEnd < 5.0 && actualEnd < duration {
			actualEnd = duration
		}

		outputPath := filepath.Join(tempDir, fmt.Sprintf("chunk_%d.wav", index))

		// 切割並轉換為 16kHz Mono 16-bit WAV (約 32,000 bytes/s)
		chunkLen := actualEnd - start
		cmd := exec.Command("ffmpeg", "-y", "-ss", strconv.FormatFloat(start, 'f', 3, 64),
			"-t", strconv.FormatFloat(chunkLen, 'f', 3, 64), "-i", inputPath,
			"-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", outputPath)

		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("failed to create chunk %d: %v", index, err)
		}

		chunks = append(chunks, Chunk{Index: index, FilePath: outputPath})
		index++

		// 靜音點切割為 clean cut，否則加入 overlap 防止斷詞
		if usedSilence || actualEnd >= duration {
			start = actualEnd
		} else {
			start = actualEnd - overlapDuration
		}

		if start >= duration {
			break
		}
	}

	return chunks, nil
}

// getSilencePoints 使用 ffmpeg silencedetect 偵測音檔中的靜音段。
// 返回每段靜音的中點時間戳，作為安全的切割候選點。
func getSilencePoints(inputPath string) ([]float64, error) {
	cmd := exec.Command("ffmpeg", "-i", inputPath, "-af", "silencedetect=noise=-30dB:d=0.5", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()

	var silences []float64
	reStart := regexp.MustCompile(`silence_start: ([\d.]+)`)
	reEnd := regexp.MustCompile(`silence_end: ([\d.]+)`)

	scanner := bufio.NewScanner(&stderr)
	var lastStart float64
	for scanner.Scan() {
		line := scanner.Text()
		if match := reStart.FindStringSubmatch(line); match != nil {
			lastStart, _ = strconv.ParseFloat(match[1], 64)
		} else if match := reEnd.FindStringSubmatch(line); match != nil {
			end, _ := strconv.ParseFloat(match[1], 64)
			// 取靜音段中點作為切割點
			silences = append(silences, (lastStart+end)/2.0)
		}
	}
	return silences, nil
}

// getDuration 使用 ffprobe 取得音檔總時長（秒）。
func getDuration(inputPath string) (float64, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", inputPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	durationStr := strings.TrimSpace(string(out))
	return strconv.ParseFloat(durationStr, 64)
}

// CleanupChunks 刪除所有分片檔案與暫存目錄。
func CleanupChunks(chunks []Chunk) {
	for _, c := range chunks {
		os.Remove(c.FilePath)
	}
	if len(chunks) > 0 {
		os.Remove(filepath.Dir(chunks[0].FilePath))
	}
}
