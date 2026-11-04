#include "backend_client.h"

#include <stdio.h>
#include <string.h>
#include "audio.h"
#include "cJSON.h"
#include "device_config.h"
#include "esp_crt_bundle.h"
#include "esp_check.h"
#include "esp_log.h"
#include "esp_websocket_client.h"
#include "freertos/FreeRTOS.h"
#include "sdkconfig.h"

#define BACKEND_MAX_FRAME_BYTES 4096

static const char *TAG = "backend";
static esp_websocket_client_handle_t s_client;
static bool s_started;
static char s_headers[256];
static uint8_t s_rx_frame[BACKEND_MAX_FRAME_BYTES];
static int s_rx_frame_size;

static void log_control_event(const char *payload, int size)
{
    cJSON *root = cJSON_ParseWithLength(payload, (size_t)size);
    if (root == NULL) {
        ESP_LOGW(TAG, "invalid JSON control frame received; bytes=%d", size);
        return;
    }
    const cJSON *type = cJSON_GetObjectItemCaseSensitive(root, "type");
    const cJSON *state = cJSON_GetObjectItemCaseSensitive(root, "state");
    const cJSON *mode = cJSON_GetObjectItemCaseSensitive(root, "mode");
    ESP_LOGI(TAG, "control event received; type=%s state=%s mode=%s",
             cJSON_IsString(type) ? type->valuestring : "unknown",
             cJSON_IsString(state) ? state->valuestring : "",
             cJSON_IsString(mode) ? mode->valuestring : "");
    cJSON_Delete(root);
}

static void on_websocket_event(void *arg, esp_event_base_t base, int32_t event_id, void *event_data)
{
    esp_websocket_event_data_t *event = event_data;
    (void)arg;
    (void)base;
    switch (event_id) {
    case WEBSOCKET_EVENT_CONNECTED:
        s_rx_frame_size = 0;
        ESP_LOGI(TAG, "backend WebSocket connected; server_session_id is managed server-side");
        break;
    case WEBSOCKET_EVENT_DISCONNECTED:
        s_rx_frame_size = 0;
        ESP_LOGW(TAG, "backend WebSocket disconnected; client will reconnect automatically");
        break;
    case WEBSOCKET_EVENT_ERROR:
        ESP_LOGE(TAG, "backend WebSocket transport error; tls=%d socket_errno=%d handshake_status=%d",
                 event->error_handle.esp_tls_last_esp_err,
                 event->error_handle.esp_transport_sock_errno,
                 event->error_handle.esp_ws_handshake_status_code);
        break;
    case WEBSOCKET_EVENT_DATA:
        if (event->payload_len < 0 || event->payload_len > BACKEND_MAX_FRAME_BYTES ||
            event->data_len < 0 || event->data_ptr == NULL || event->payload_offset < 0 ||
            event->payload_offset > BACKEND_MAX_FRAME_BYTES - event->data_len) {
            s_rx_frame_size = 0;
            ESP_LOGW(TAG, "oversized backend frame discarded; payload_bytes=%d", event->payload_len);
            break;
        }
        if (event->payload_offset == 0) {
            s_rx_frame_size = event->payload_len;
        }
        if (event->payload_offset + event->data_len > s_rx_frame_size) {
            s_rx_frame_size = 0;
            ESP_LOGW(TAG, "backend frame chunk exceeds declared payload; chunk_offset=%d chunk_bytes=%d",
                     event->payload_offset, event->data_len);
            break;
        }
        memcpy(s_rx_frame + event->payload_offset, event->data_ptr, event->data_len);
        // WebSocket 库可能拆分大消息；只在完整收齐后区分二进制音频和 JSON 控制事件。
        if (event->fin && event->payload_offset + event->data_len == s_rx_frame_size) {
            if (event->op_code == 0x2) {
                audio_play_frame(s_rx_frame, (size_t)s_rx_frame_size);
            } else if (event->op_code == 0x1) {
                log_control_event((const char *)s_rx_frame, s_rx_frame_size);
            }
            s_rx_frame_size = 0;
        }
        break;
    default:
        break;
    }
}

esp_err_t backend_client_start(void)
{
    if (s_started) {
        ESP_LOGI(TAG, "backend WebSocket start ignored; client is already initialized");
        return ESP_OK;
    }
    const device_config_t *config = device_config_get();
    ESP_RETURN_ON_FALSE(config->device_token[0] != '\0', ESP_ERR_INVALID_STATE, TAG,
                        "backend token is not configured");

    int header_size = snprintf(s_headers, sizeof(s_headers),
                               "Device-Id: %s\r\nAuthorization: Bearer %s\r\n",
                               config->device_id, config->device_token);
    ESP_RETURN_ON_FALSE(header_size > 0 && (size_t)header_size < sizeof(s_headers), ESP_ERR_INVALID_SIZE, TAG,
                        "backend request headers exceed buffer");

    esp_websocket_client_config_t ws_config = {
        .uri = config->backend_uri,
        .headers = s_headers,
        .buffer_size = 4096,
        .reconnect_timeout_ms = 1000,
        .network_timeout_ms = 10000,
        .crt_bundle_attach = esp_crt_bundle_attach,
    };
    s_client = esp_websocket_client_init(&ws_config);
    ESP_RETURN_ON_FALSE(s_client != NULL, ESP_ERR_NO_MEM, TAG, "create backend WebSocket client failed");
    ESP_RETURN_ON_ERROR(esp_websocket_register_events(s_client, WEBSOCKET_EVENT_ANY, on_websocket_event, NULL),
                        TAG, "register backend WebSocket events failed");
    ESP_RETURN_ON_ERROR(esp_websocket_client_start(s_client), TAG, "start backend WebSocket failed");
    s_started = true;
    // Go 服务握手成功后不要求额外 hello；它会按 Device-Id 绑定独立 Linx 会话。
    ESP_LOGI(TAG, "backend WebSocket client started; device_id=%s reconnect_ms=%d",
             config->device_id, ws_config.reconnect_timeout_ms);
    return ESP_OK;
}

bool backend_client_send_audio(const uint8_t *pcm, size_t size)
{
    if (!s_started || !esp_websocket_client_is_connected(s_client)) {
        return false;
    }
    int sent = esp_websocket_client_send_bin(s_client, (const char *)pcm, (int)size, pdMS_TO_TICKS(100));
    if (sent != (int)size) {
        ESP_LOGW(TAG, "microphone frame send failed; expected_bytes=%u sent_bytes=%d",
                 (unsigned)size, sent);
        return false;
    }
    return true;
}
