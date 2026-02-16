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

// SplitAudio 將音檔切割為符合 STT 模型限制的分片。
//
// 策略：
//   - 小於 25MB 的檔案直接轉換格式，不切割
//   - VAD 優先：使用 ffmpeg silencedetect 偵測 ≥0.5s 靜音段，在靜音中點切割
//   - Overlap Fallback：目標點 ±5s 內無靜音段時，銜接處加入 1.5s 重疊避免斷詞
//   - 格式標準化：所有分片統一轉換為 16kHz Mono WAV
func SplitAudio(inputPath string, maxChunkDuration float64) ([]Chunk, error) {
	tempDir := filepath.Join(filepath.Dir(inputPath), "chunks")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, err
	}

	// 小於 25MB 直接作為單一分片（符合 Whisper API 限制）
	info, err := os.Stat(inputPath)
	if err != nil {
		return nil, err
	}
	if info.Size() < 25*1024*1024 {
		outputPath := filepath.Join(tempDir, "chunk_0.wav")
		cmd := exec.Command("ffmpeg", "-y", "-i", inputPath, "-ar", "16000", "-ac", "1", outputPath)
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("failed to convert audio: %v", err)
		}
		return []Chunk{{Index: 0, FilePath: outputPath}}, nil
	}

	duration, err := getDuration(inputPath)
	if err != nil {
		return nil, err
	}

	silences, err := getSilencePoints(inputPath)
	if err != nil {
		// VAD 失敗時退化為固定長度切割
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

		// 在目標切割點 ±5s 內搜尋最近的靜音點
		actualEnd := targetEnd
		usedSilence := false
		if targetEnd < duration {
			bestSilence := -1.0
			minDiff := 5.0

			for _, s := range silences {
				if s <= start {
					continue
				}
				diff := s - targetEnd
				if diff < 0 {
					diff = -diff
				}
				if diff < minDiff {
					minDiff = diff
					bestSilence = s
				}
			}

			if bestSilence != -1.0 {
				actualEnd = bestSilence
				usedSilence = true
			}
		}

		// 避免尾端產生過短分片（<5s 直接合併至當前分片）
		if duration-actualEnd < 5.0 && actualEnd < duration {
			actualEnd = duration
		}

		outputPath := filepath.Join(tempDir, fmt.Sprintf("chunk_%d.wav", index))

		// 切割並轉換為 16kHz Mono WAV
		chunkLen := actualEnd - start
		cmd := exec.Command("ffmpeg", "-y", "-ss", strconv.FormatFloat(start, 'f', 3, 64),
			"-t", strconv.FormatFloat(chunkLen, 'f', 3, 64), "-i", inputPath,
			"-ar", "16000", "-ac", "1", outputPath)

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
