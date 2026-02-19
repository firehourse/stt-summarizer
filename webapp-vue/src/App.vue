<script setup>
import { ref, onUnmounted } from "vue";
import axios from "axios";
import {
  Upload,
  FileAudio,
  CheckCircle,
  AlertCircle,
  Loader2,
  X,
  RefreshCw,
} from "lucide-vue-next";

const fileInput = ref(null);
const isDragging = ref(false);
const currentTask = ref(null);
const eventSource = ref(null);
const uploadProgress = ref(0);
const isUploading = ref(false);

const handleDrop = (e) => {
  isDragging.value = false;
  const files = e.dataTransfer.files;
  if (files.length) uploadFile(files[0]);
};

const triggerFileInput = () => fileInput.value.click();

const handleFileChange = (e) => {
  if (e.target.files.length) uploadFile(e.target.files[0]);
};

const uploadFile = async (file) => {
  isUploading.value = true;
  uploadProgress.value = 0;

  try {
    // 1. Pre-register (POST)
    const { data: preData } = await axios.post("/api/tasks");
    const taskId = preData.taskId;

    currentTask.value = {
      id: taskId,
      name: file.name,
      status: "pending",
      progress: 0,
      message: "上傳中...",
      transcript: "",
      summary: "",
    };

    // 2. Stream Upload (PUT)
    const formData = new FormData();
    formData.append("file", file);

    await axios.put(`/api/tasks/${taskId}/upload`, formData, {
      onUploadProgress: (progressEvent) => {
        uploadProgress.value = Math.round(
          (progressEvent.loaded * 100) / progressEvent.total,
        );
      },
    });

    currentTask.value.status = "processing";
    currentTask.value.message = "上傳完成，開始處理...";
    isUploading.value = false;

    startListening(taskId);
  } catch (err) {
    console.error("Upload failed", err);
    if (currentTask.value) {
      currentTask.value.status = "failed";
      currentTask.value.message = err.response?.data?.error || err.message;
    }
    isUploading.value = false;
  }
};

const startListening = (taskId) => {
  if (eventSource.value) eventSource.value.close();

  eventSource.value = new EventSource(`/api/tasks/${taskId}/events`);

  eventSource.value.onmessage = async (event) => {
    const data = JSON.parse(event.data);

    if (data.type === "summary_chunk") {
      // 摘要是增量推送 (Chunk-based)
      currentTask.value.summary =
        (currentTask.value.summary || "") + data.content;
      currentTask.value.message = "摘要生成中...";
    } else if (data.type === "transcript_update") {
      // 逐字稿是全量累積推送 (Cumulative)
      currentTask.value.transcript = data.content;
      currentTask.value.message = "語音轉譯中...";
    } else if (data.type === "completed") {
      currentTask.value.status = "completed";
      currentTask.value.progress = 100;
      currentTask.value.message = "完成";
      // Fetch final transcript + summary from DB
      try {
        const res = await axios.get(`/api/tasks/${taskId}`);
        currentTask.value.transcript = res.data.transcript;
        // Use DB summary if streaming didn't capture everything
        if (!currentTask.value.summary && res.data.summary) {
          currentTask.value.summary = res.data.summary;
        }
      } catch (e) {
        console.error("Failed to fetch result", e);
      }
      eventSource.value.close();
    } else if (data.type === "failed" || data.type === "cancelled") {
      currentTask.value.status = data.type;
      currentTask.value.message = data.message || "Task failed";
      eventSource.value.close();
    } else {
      // progress event
      currentTask.value.status = data.status || "processing";
      currentTask.value.progress = data.progress || 0;
      currentTask.value.message = data.message || "";
    }
  };

  eventSource.value.onerror = () => {
    console.warn("SSE connection error, will auto-reconnect");
  };
};

const requestResummarize = async () => {
  if (!currentTask.value) return;
  currentTask.value.status = "processing";
  currentTask.value.summary = "";
  currentTask.value.message = "重新摘要中...";

  try {
    await axios.post(`/api/tasks/${currentTask.value.id}/summarize`);
    startListening(currentTask.value.id);
  } catch (err) {
    currentTask.value.status = "failed";
    currentTask.value.message = "Failed to request re-summary: " + err.message;
  }
};

const cancelTask = async () => {
  if (!currentTask.value) return;
  try {
    await axios.delete(`/api/tasks/${currentTask.value.id}`);
    currentTask.value.status = "cancelled";
    currentTask.value.message = "已取消";
    if (eventSource.value) eventSource.value.close();
  } catch (err) {
    console.error("Cancel failed", err);
  }
};

onUnmounted(() => {
  if (eventSource.value) eventSource.value.close();
});
</script>

<template>
  <div class="max-w-4xl mx-auto py-12 px-4 sm:px-6 lg:px-8">
    <header class="text-center mb-12">
      <h1
        class="text-5xl font-extrabold tracking-tight text-transparent bg-clip-text bg-gradient-to-r from-indigo-400 to-purple-400 mb-4"
      >
        STT Summarizer
      </h1>
      <p class="text-lg text-slate-400">
        Upload audio, get transcription & AI summary — powered by a decoupled
        pipeline.
      </p>
    </header>

    <main class="space-y-8">
      <!-- Upload Section -->
      <div
        @dragover.prevent="isDragging = true"
        @dragleave.prevent="isDragging = false"
        @drop.prevent="handleDrop"
        @click="triggerFileInput"
        :class="[
          'relative border-2 border-dashed rounded-3xl p-12 text-center transition-all cursor-pointer group',
          isDragging
            ? 'border-indigo-500 bg-indigo-500/10'
            : 'border-slate-700 hover:border-slate-500 hover:bg-slate-800/10',
        ]"
      >
        <input
          type="file"
          ref="fileInput"
          class="hidden"
          accept="audio/*,video/*"
          @change="handleFileChange"
        />

        <div class="flex flex-col items-center">
          <div
            class="p-4 bg-indigo-500/20 rounded-2xl mb-6 group-hover:scale-110 transition-transform"
          >
            <Upload class="w-10 h-10 text-indigo-400" />
          </div>
          <p class="text-xl font-medium text-slate-200 mb-2">
            點擊或拖放上傳音檔
          </p>
          <p class="text-sm text-slate-500">支援 MP3, WAV, M4A，最大 1GB</p>
        </div>
      </div>

      <!-- Active Task Section -->
      <transition
        enter-active-class="transition duration-300 ease-out"
        enter-from-class="transform translate-y-4 opacity-0"
        enter-to-class="transform translate-y-0 opacity-100"
      >
        <div
          v-if="currentTask || isUploading"
          class="bg-slate-800/40 backdrop-blur-xl border border-slate-700/50 rounded-3xl p-8 shadow-2xl"
        >
          <div class="flex items-start justify-between mb-8">
            <div class="flex items-center gap-4">
              <div class="p-3 bg-indigo-500/10 rounded-xl">
                <FileAudio class="w-6 h-6 text-indigo-400" />
              </div>
              <div>
                <h3
                  class="text-xl font-semibold text-slate-100 truncate max-w-xs"
                >
                  {{ currentTask?.name || "Uploading file..." }}
                </h3>
                <div class="flex items-center gap-2 mt-1">
                  <span
                    :class="[
                      'px-2.5 py-0.5 rounded-full text-xs font-bold uppercase tracking-wider',
                      currentTask?.status === 'completed'
                        ? 'bg-emerald-500/20 text-emerald-400'
                        : currentTask?.status === 'failed' ||
                            currentTask?.status === 'cancelled'
                          ? 'bg-rose-500/20 text-rose-400'
                          : 'bg-sky-500/20 text-sky-400',
                    ]"
                  >
                    {{ isUploading ? "上傳中" : currentTask?.status }}
                  </span>
                </div>
              </div>
            </div>

            <button
              v-if="
                !['completed', 'failed', 'cancelled'].includes(
                  currentTask?.status,
                ) && !isUploading
              "
              @click="cancelTask"
              class="p-2 hover:bg-rose-500/10 rounded-full text-slate-400 hover:text-rose-400 transition-colors"
              title="取消任務"
            >
              <X class="w-6 h-6" />
            </button>
          </div>

          <!-- Progress -->
          <div class="space-y-3">
            <div class="flex justify-between text-sm font-medium">
              <span class="text-slate-400">
                {{ isUploading ? "檔案上傳" : currentTask?.message }}
              </span>
              <span class="text-indigo-400 font-mono">
                {{ isUploading ? uploadProgress : currentTask?.progress }}%
              </span>
            </div>
            <div class="h-1.5 w-full bg-slate-700 rounded-full overflow-hidden">
              <div
                class="h-full bg-gradient-to-r from-indigo-500 to-purple-500 transition-all duration-500"
                :style="{
                  width: `${isUploading ? uploadProgress : currentTask?.progress}%`,
                }"
              ></div>
            </div>
          </div>

          <!-- Streaming Results (visible during processing) -->
          <div
            v-if="
              currentTask?.status === 'processing' &&
              (currentTask?.transcript || currentTask?.summary)
            "
            class="mt-8 space-y-6 animate-in"
          >
            <!-- Streaming Transcript -->
            <div v-if="currentTask?.transcript" class="space-y-3">
              <h4
                class="text-sm font-bold text-slate-500 uppercase flex items-center gap-2"
              >
                <Loader2 class="w-4 h-4 animate-spin" /> 語音轉譯中
              </h4>
              <div
                class="bg-slate-900/50 rounded-2xl p-6 border border-slate-700/30 text-slate-300 leading-relaxed max-h-40 overflow-y-auto"
              >
                {{ currentTask.transcript
                }}<span class="animate-pulse text-indigo-400">|</span>
              </div>
            </div>

            <!-- Streaming Summary -->
            <div v-if="currentTask?.summary" class="space-y-3">
              <h4
                class="text-sm font-bold text-slate-500 uppercase flex items-center gap-2"
              >
                <Loader2 class="w-4 h-4 animate-spin" /> 摘要生成中
              </h4>
              <div
                class="bg-indigo-500/5 rounded-2xl p-6 border border-indigo-500/20 text-indigo-50/90 leading-relaxed whitespace-pre-wrap"
              >
                {{ currentTask.summary }}<span class="animate-pulse">▊</span>
              </div>
            </div>
          </div>

          <!-- Final Results -->
          <div
            v-if="currentTask?.status === 'completed'"
            class="mt-10 grid gap-6 animate-in"
          >
            <div class="space-y-3">
              <h4
                class="text-sm font-bold text-slate-500 uppercase flex items-center gap-2"
              >
                <CheckCircle class="w-4 h-4" /> 轉錄結果
              </h4>
              <div
                class="bg-slate-900/50 rounded-2xl p-6 border border-slate-700/30 text-slate-300 leading-relaxed max-h-60 overflow-y-auto"
              >
                {{ currentTask.transcript }}
              </div>
            </div>

            <div v-if="currentTask.summary" class="space-y-3">
              <div class="flex items-center justify-between">
                <h4
                  class="text-sm font-bold text-slate-500 uppercase flex items-center gap-2"
                >
                  <CheckCircle class="w-4 h-4" /> 摘要
                </h4>
                <button
                  @click="requestResummarize"
                  class="text-xs font-bold text-indigo-400 hover:text-indigo-300 flex items-center gap-1 transition-colors bg-indigo-500/10 px-3 py-1 rounded-lg border border-indigo-500/20"
                >
                  <RefreshCw class="w-3 h-3" />
                  重新摘要
                </button>
              </div>
              <div
                class="bg-indigo-500/5 rounded-2xl p-6 border border-indigo-500/20 text-indigo-50/90 leading-relaxed whitespace-pre-wrap"
              >
                {{ currentTask.summary }}
              </div>
            </div>
          </div>

          <!-- Error Message -->
          <div
            v-if="
              currentTask?.status === 'failed' ||
              currentTask?.status === 'cancelled'
            "
            class="mt-6 flex items-center gap-3 p-4 bg-rose-500/10 border border-rose-500/20 rounded-2xl text-rose-400"
          >
            <AlertCircle class="w-5 h-5 flex-shrink-0" />
            <p class="text-sm font-medium">{{ currentTask.message }}</p>
          </div>
        </div>
      </transition>
    </main>

    <footer class="mt-16 text-center text-slate-500 text-sm">
      <p>&copy; 2026 STT-AI</p>
    </footer>
  </div>
</template>

<style scoped>
.animate-in {
  animation: fadeIn 0.5s ease-out forwards;
}

@keyframes fadeIn {
  from {
    opacity: 0;
    transform: translateY(10px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}
</style>
