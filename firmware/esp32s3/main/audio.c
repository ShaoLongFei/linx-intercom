#include "audio.h"

#include <string.h>
#include "driver/gpio.h"
#include "driver/i2s_std.h"
#include "esp_check.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"
#include "sdkconfig.h"

#define AUDIO_SAMPLE_RATE 16000
#define AUDIO_FRAME_SAMPLES 320
#define AUDIO_FRAME_BYTES (AUDIO_FRAME_SAMPLES * sizeof(int16_t))
#define AUDIO_PLAY_QUEUE_DEPTH 8
#define AUDIO_DMA_DESC_NUM 6
#define AUDIO_DMA_FRAME_SAMPLES 240
#define AUDIO_DMA_DRAIN_MS ((AUDIO_DMA_DESC_NUM * AUDIO_DMA_FRAME_SAMPLES * 1000 / AUDIO_SAMPLE_RATE) + 10)

// Go 后端按 20 ms PCM16 mono @ 16 kHz 二进制消息收发，每帧固定 640 字节。
typedef struct {
    size_t size;
    bool drain_marker;
    uint8_t pcm[AUDIO_FRAME_BYTES];
} audio_frame_t;

static const char *TAG = "audio";
static i2s_chan_handle_t s_rx_channel;
static i2s_chan_handle_t s_tx_channel;
static QueueHandle_t s_play_queue;
static audio_send_frame_fn s_send_frame;
static audio_playback_drained_fn s_playback_drained;
static TickType_t s_last_invalid_frame_log;

static bool write_i2s_tx(const uint8_t *data, size_t size)
{
    size_t offset = 0;
    while (offset < size) {
        size_t bytes_written = 0;
        esp_err_t err = i2s_channel_write(s_tx_channel, data + offset, size - offset, &bytes_written,
                                          pdMS_TO_TICKS(100));
        if (bytes_written > 0) {
            offset += bytes_written;
        }
        if (err != ESP_OK || bytes_written == 0) {
            ESP_LOGW(TAG, "I2S playback write incomplete; error=%s bytes=%u/%u",
                     esp_err_to_name(err), (unsigned)offset, (unsigned)size);
            return false;
        }
    }
    return true;
}

esp_err_t audio_init(void)
{
    i2s_chan_config_t channel_config = I2S_CHANNEL_DEFAULT_CONFIG(I2S_NUM_0, I2S_ROLE_MASTER);
    channel_config.dma_desc_num = AUDIO_DMA_DESC_NUM;
    channel_config.dma_frame_num = AUDIO_DMA_FRAME_SAMPLES;
    ESP_RETURN_ON_ERROR(i2s_new_channel(&channel_config, &s_tx_channel, &s_rx_channel), TAG,
                        "create I2S channels failed");

    i2s_std_config_t standard_config = {
        .clk_cfg = I2S_STD_CLK_DEFAULT_CONFIG(AUDIO_SAMPLE_RATE),
        .slot_cfg = I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_MONO),
        .gpio_cfg = {
            .mclk = CONFIG_INTERCOM_I2S_MCLK_GPIO < 0 ? I2S_GPIO_UNUSED : CONFIG_INTERCOM_I2S_MCLK_GPIO,
            .bclk = CONFIG_INTERCOM_I2S_BCLK_GPIO,
            .ws = CONFIG_INTERCOM_I2S_WS_GPIO,
            .dout = CONFIG_INTERCOM_I2S_DOUT_GPIO,
            .din = CONFIG_INTERCOM_I2S_DIN_GPIO,
            .invert_flags = {.mclk_inv = false, .bclk_inv = false, .ws_inv = false},
        },
    };
    ESP_RETURN_ON_ERROR(i2s_channel_init_std_mode(s_rx_channel, &standard_config), TAG,
                        "initialize I2S receive channel failed");
    ESP_RETURN_ON_ERROR(i2s_channel_init_std_mode(s_tx_channel, &standard_config), TAG,
                        "initialize I2S transmit channel failed");

    // Codec 的静音/增益寄存器因板型不同而异，这里只负责标准 I2S 数字通道。
    if (CONFIG_INTERCOM_AUDIO_AMPLIFIER_GPIO >= 0) {
        gpio_config_t gpio_config = {
            .pin_bit_mask = 1ULL << CONFIG_INTERCOM_AUDIO_AMPLIFIER_GPIO,
            .mode = GPIO_MODE_OUTPUT,
        };
        ESP_RETURN_ON_ERROR(gpio_config(&gpio_config), TAG, "configure amplifier GPIO failed");
        ESP_RETURN_ON_ERROR(gpio_set_level(CONFIG_INTERCOM_AUDIO_AMPLIFIER_GPIO,
                                           CONFIG_INTERCOM_AUDIO_AMPLIFIER_ACTIVE_LEVEL), TAG,
                            "enable amplifier failed");
    }
    s_play_queue = xQueueCreate(AUDIO_PLAY_QUEUE_DEPTH, sizeof(audio_frame_t));
    ESP_RETURN_ON_FALSE(s_play_queue != NULL, ESP_ERR_NO_MEM, TAG, "create audio playback queue failed");
    ESP_LOGI(TAG, "I2S initialized; sample_rate=%d frame_ms=20 frame_bytes=%u",
             AUDIO_SAMPLE_RATE, (unsigned)AUDIO_FRAME_BYTES);
    return ESP_OK;
}

static void capture_task(void *arg)
{
    uint8_t frame[AUDIO_FRAME_BYTES];
    (void)arg;
    ESP_LOGI(TAG, "microphone capture task started");
    while (true) {
        size_t bytes_read = 0;
        esp_err_t err = i2s_channel_read(s_rx_channel, frame, sizeof(frame), &bytes_read,
                                         pdMS_TO_TICKS(100));
        if (err != ESP_OK) {
            ESP_LOGW(TAG, "I2S capture read failed: %s", esp_err_to_name(err));
            continue;
        }
        if (bytes_read != sizeof(frame)) {
            ESP_LOGW(TAG, "incomplete microphone frame dropped; bytes=%u", (unsigned)bytes_read);
            continue;
        }
        // 断网期间不缓存麦克风帧，避免重连后把过期语音送进对讲或控制通道。
        if (s_send_frame != NULL && !s_send_frame(frame, bytes_read)) {
            vTaskDelay(pdMS_TO_TICKS(20));
        }
    }
}

static void playback_task(void *arg)
{
    audio_frame_t frame;
    (void)arg;
    ESP_LOGI(TAG, "speaker playback task started");
    while (true) {
        if (xQueueReceive(s_play_queue, &frame, portMAX_DELAY) != pdTRUE) {
            continue;
        }
        if (frame.drain_marker) {
            static const uint8_t silence[AUDIO_FRAME_BYTES] = {0};
            // write 只表示数据进入 DMA；完整 DMA 环最多积压 90 ms，等待 100 ms 后再确认尾音送完。
            while (true) {
                if (write_i2s_tx(silence, sizeof(silence))) {
                    vTaskDelay(pdMS_TO_TICKS(AUDIO_DMA_DRAIN_MS));
                    if (s_playback_drained != NULL) {
                        s_playback_drained();
                    }
                    break;
                }
                vTaskDelay(pdMS_TO_TICKS(100));
            }
            continue;
        }
        (void)write_i2s_tx(frame.pcm, frame.size);
    }
}

esp_err_t audio_start(audio_send_frame_fn send_frame, audio_playback_drained_fn playback_drained)
{
    ESP_RETURN_ON_FALSE(send_frame != NULL, ESP_ERR_INVALID_ARG, TAG, "audio sender is required");
    ESP_RETURN_ON_FALSE(playback_drained != NULL, ESP_ERR_INVALID_ARG, TAG,
                        "playback drain acknowledgement sender is required");
    s_send_frame = send_frame;
    s_playback_drained = playback_drained;
    ESP_RETURN_ON_ERROR(i2s_channel_enable(s_rx_channel), TAG, "enable I2S receive channel failed");
    ESP_RETURN_ON_ERROR(i2s_channel_enable(s_tx_channel), TAG, "enable I2S transmit channel failed");
    BaseType_t ok = xTaskCreate(capture_task, "mic_capture", 4096, NULL, 6, NULL);
    ESP_RETURN_ON_FALSE(ok == pdPASS, ESP_ERR_NO_MEM, TAG, "start microphone task failed");
    ok = xTaskCreate(playback_task, "speaker_playback", 3072, NULL, 5, NULL);
    ESP_RETURN_ON_FALSE(ok == pdPASS, ESP_ERR_NO_MEM, TAG, "start speaker task failed");
    return ESP_OK;
}

void audio_play_frame(const uint8_t *pcm, size_t size)
{
    if (pcm == NULL || size != AUDIO_FRAME_BYTES || s_play_queue == NULL) {
        TickType_t now = xTaskGetTickCount();
        if (now - s_last_invalid_frame_log > pdMS_TO_TICKS(5000)) {
            ESP_LOGW(TAG, "invalid downlink audio frame discarded; size=%u", (unsigned)size);
            s_last_invalid_frame_log = now;
        }
        return;
    }
    audio_frame_t frame = {.size = size};
    memcpy(frame.pcm, pcm, size);
    if (xQueueSend(s_play_queue, &frame, 0) == pdTRUE) {
        return;
    }

    audio_frame_t stale;
    bool dropped_oldest = xQueueReceive(s_play_queue, &stale, 0) == pdTRUE;
    if (dropped_oldest && stale.drain_marker) {
        (void)xQueueSendToFront(s_play_queue, &stale, 0);
        ESP_LOGW(TAG, "speaker queue full after playback marker; incoming audio frame dropped");
        return;
    }
    if (xQueueSend(s_play_queue, &frame, 0) != pdTRUE) {
        ESP_LOGW(TAG, "speaker queue remained full; incoming audio frame dropped");
        return;
    }
    if (dropped_oldest) {
        ESP_LOGW(TAG, "speaker queue full; oldest audio frame dropped");
    }
}

void audio_wait_for_playback_drain(void)
{
    audio_frame_t marker = {.drain_marker = true};
    if (s_play_queue == NULL || xQueueSend(s_play_queue, &marker, pdMS_TO_TICKS(1000)) != pdTRUE) {
        ESP_LOGW(TAG, "playback-drained marker could not be queued");
        return;
    }
    ESP_LOGI(TAG, "playback-drained marker queued behind pending audio");
}

void audio_clear_playback(void)
{
    if (s_play_queue != NULL) {
        // 正常 TTS 停止等待此队列和当前 I2S 写入完成；控制切换/打断才清空排队帧。
        UBaseType_t pending = uxQueueMessagesWaiting(s_play_queue);
        xQueueReset(s_play_queue);
        ESP_LOGI(TAG, "speaker playback queue cleared; pending_frames=%u", (unsigned)pending);
    }
}
