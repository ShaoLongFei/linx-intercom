#include "backend_client.h"

#include <stdio.h>
#include <string.h>
#include "audio.h"
#include "esp_app_desc.h"
#include "cJSON.h"
#include "device_config.h"
#include "esp_crt_bundle.h"
#include "esp_check.h"
#include "esp_flash.h"
#include "esp_partition.h"
#include "esp_log.h"
#include "esp_mac.h"
#include "esp_system.h"
#include "esp_wifi.h"
#include "esp_websocket_client.h"
#include "freertos/FreeRTOS.h"
#include "sdkconfig.h"

#define BACKEND_MAX_FRAME_BYTES 4096
#define DEVICE_INFO_MAX_BYTES (16 * 1024)

static const char *TAG = "backend";
static esp_websocket_client_handle_t s_client;
static bool s_started;
static char s_headers[256];
static uint8_t s_rx_frame[BACKEND_MAX_FRAME_BYTES];
static int s_rx_frame_size;
static portMUX_TYPE s_playback_ack_mux = portMUX_INITIALIZER_UNLOCKED;
static bool s_playback_ack_pending;
static bool s_playback_ack_sending;

static void add_partition_table(cJSON *report)
{
    cJSON *partitions = cJSON_CreateArray();
    if (partitions == NULL) {
        ESP_LOGE(TAG, "failed to allocate OTA partition metadata");
        return;
    }
    cJSON_AddItemToObject(report, "partition_table", partitions);
    esp_partition_iterator_t iterator = esp_partition_find(ESP_PARTITION_TYPE_ANY,
                                                           ESP_PARTITION_SUBTYPE_ANY, NULL);
    while (iterator != NULL) {
        const esp_partition_t *partition = esp_partition_get(iterator);
        cJSON *item = cJSON_CreateObject();
        if (item == NULL) {
            ESP_LOGE(TAG, "failed to allocate OTA partition entry");
            break;
        }
        cJSON_AddStringToObject(item, "label", partition->label);
        cJSON_AddNumberToObject(item, "type", partition->type);
        cJSON_AddNumberToObject(item, "subtype", partition->subtype);
        cJSON_AddNumberToObject(item, "address", partition->address);
        cJSON_AddNumberToObject(item, "size", partition->size);
        cJSON_AddItemToArray(partitions, item);
        iterator = esp_partition_next(iterator);
    }
    esp_partition_iterator_release(iterator);
}

static void send_device_info(void)
{
    const device_config_t *config = device_config_get();
    const esp_app_desc_t *app = esp_app_get_description();
    wifi_ap_record_t ap = {0};
    esp_err_t err = esp_wifi_sta_get_ap_info(&ap);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "read Wi-Fi status for OTA metadata failed; error=%s", esp_err_to_name(err));
        return;
    }

    uint8_t mac[6];
    err = esp_read_mac(mac, ESP_MAC_WIFI_STA);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "read station MAC for OTA metadata failed; error=%s", esp_err_to_name(err));
        return;
    }
    char mac_address[18];
    snprintf(mac_address, sizeof(mac_address), "%02X:%02X:%02X:%02X:%02X:%02X",
             mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
    if (strcmp(mac_address, config->device_id) != 0) {
        ESP_LOGE(TAG, "OTA metadata MAC mismatch; configured_device_id=%s sta_mac=%s",
                 config->device_id, mac_address);
        return;
    }

    char elf_sha256[65];
    for (size_t i = 0; i < sizeof(app->app_elf_sha256); ++i) {
        snprintf(elf_sha256 + i * 2, 3, "%02x", app->app_elf_sha256[i]);
    }
    elf_sha256[64] = '\0';
    char compile_time[40];
    snprintf(compile_time, sizeof(compile_time), "%s %s", app->date, app->time);

    esp_chip_info_t chip = {0};
    esp_chip_info(&chip);
    uint32_t flash_size = 0;
    err = esp_flash_get_size(NULL, &flash_size);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "read flash size for OTA metadata failed; error=%s", esp_err_to_name(err));
        return;
    }

    cJSON *root = cJSON_CreateObject();
    if (root == NULL) {
        ESP_LOGE(TAG, "failed to allocate OTA device metadata JSON");
        return;
    }
    cJSON_AddStringToObject(root, "type", "device_info");
    cJSON *info = cJSON_CreateObject();
    if (info == NULL) {
        ESP_LOGE(TAG, "failed to allocate OTA device metadata payload");
        goto cleanup;
    }
    cJSON_AddItemToObject(root, "device_info", info);
    cJSON *report = cJSON_CreateObject();
    if (report == NULL) {
        ESP_LOGE(TAG, "failed to allocate OTA device report");
        goto cleanup;
    }
    cJSON_AddItemToObject(info, "report", report);
    cJSON *application = cJSON_CreateObject();
    cJSON *board = cJSON_CreateObject();
    cJSON *chip_info = cJSON_CreateObject();
    if (application == NULL || board == NULL || chip_info == NULL) {
        cJSON_Delete(application);
        cJSON_Delete(board);
        cJSON_Delete(chip_info);
        ESP_LOGE(TAG, "failed to allocate OTA device metadata section");
        goto cleanup;
    }

    int activation_version = CONFIG_INTERCOM_SERIAL_NUMBER[0] == '\0' ? 1 : 2;
    cJSON_AddNumberToObject(info, "activation_version", activation_version);
    if (activation_version == 2) {
        cJSON_AddStringToObject(info, "serial_number", CONFIG_INTERCOM_SERIAL_NUMBER);
    }
    cJSON_AddStringToObject(application, "name", app->project_name);
    cJSON_AddStringToObject(application, "version", app->version);
    cJSON_AddStringToObject(application, "compile_time", compile_time);
    cJSON_AddStringToObject(application, "idf_version", app->idf_ver);
    cJSON_AddStringToObject(application, "elf_sha256", elf_sha256);
    cJSON_AddStringToObject(board, "type", "linx-intercom-esp32s3");
    cJSON_AddStringToObject(board, "ssid", (const char *)ap.ssid);
    cJSON_AddNumberToObject(board, "rssi", ap.rssi);
    cJSON_AddNumberToObject(board, "channel", ap.primary);
    cJSON_AddStringToObject(board, "mac", mac_address);
    cJSON_AddItemToObject(report, "application", application);
    cJSON_AddItemToObject(report, "board", board);
    cJSON_AddItemToObject(report, "chip_info", chip_info);
    cJSON_AddStringToObject(report, "mac_address", mac_address);
    cJSON_AddStringToObject(report, "chip_model_name", "esp32s3");
    cJSON_AddNumberToObject(report, "flash_size", flash_size);
    cJSON_AddNumberToObject(chip_info, "model", chip.model);
    cJSON_AddNumberToObject(chip_info, "cores", chip.cores);
    cJSON_AddNumberToObject(chip_info, "revision", chip.revision);
    cJSON_AddNumberToObject(chip_info, "features", chip.features);
    add_partition_table(report);

    char *payload = cJSON_PrintUnformatted(root);
    if (payload == NULL) {
        ESP_LOGE(TAG, "serialize OTA device metadata failed");
        goto cleanup;
    }
    size_t payload_size = strlen(payload);
    if (payload_size >= DEVICE_INFO_MAX_BYTES) {
        ESP_LOGE(TAG, "OTA device metadata exceeds limit; bytes=%u", (unsigned)payload_size);
        cJSON_free(payload);
        goto cleanup;
    }
    // 原样记录完整 OTA 元数据便于调试；其中不包含 Wi-Fi 密码或设备 bearer token。
    ESP_LOGI(TAG, "sending OTA device metadata; json=%s", payload);
    int sent = esp_websocket_client_send_text(s_client, payload, (int)payload_size, pdMS_TO_TICKS(1000));
    if (sent != (int)payload_size) {
        ESP_LOGE(TAG, "OTA device metadata send failed; expected_bytes=%u sent_bytes=%d",
                 (unsigned)payload_size, sent);
    } else {
        ESP_LOGI(TAG, "OTA device metadata sent; device_id=%s bytes=%u", config->device_id, (unsigned)payload_size);
    }
    cJSON_free(payload);
cleanup:
    cJSON_Delete(root);
}

static void send_pending_playback_ack(void)
{
    static const char event[] = "{\"type\":\"playback\",\"state\":\"drained\"}";
    portENTER_CRITICAL(&s_playback_ack_mux);
    bool should_send = s_playback_ack_pending && !s_playback_ack_sending;
    if (should_send) {
        s_playback_ack_sending = true;
    }
    portEXIT_CRITICAL(&s_playback_ack_mux);
    if (!should_send) {
        return;
    }
    if (!s_started || !esp_websocket_client_is_connected(s_client)) {
        portENTER_CRITICAL(&s_playback_ack_mux);
        s_playback_ack_sending = false;
        portEXIT_CRITICAL(&s_playback_ack_mux);
        ESP_LOGW(TAG, "playback-drained acknowledgement deferred; backend is disconnected");
        return;
    }
    int sent = esp_websocket_client_send_text(s_client, event, sizeof(event) - 1, pdMS_TO_TICKS(100));
    portENTER_CRITICAL(&s_playback_ack_mux);
    s_playback_ack_sending = false;
    portEXIT_CRITICAL(&s_playback_ack_mux);
    if (sent != (int)(sizeof(event) - 1)) {
        ESP_LOGW(TAG, "playback-drained acknowledgement failed; expected_bytes=%u sent_bytes=%d",
                 (unsigned)(sizeof(event) - 1), sent);
        return;
    }
    ESP_LOGI(TAG, "playback-drained acknowledgement sent; waiting for room-mode confirmation");
}

static void handle_control_event(const char *payload, int size)
{
    cJSON *root = cJSON_ParseWithLength(payload, (size_t)size);
    if (root == NULL) {
        ESP_LOGW(TAG, "invalid JSON control frame received; bytes=%d", size);
        return;
    }
    const cJSON *type = cJSON_GetObjectItemCaseSensitive(root, "type");
    const cJSON *state = cJSON_GetObjectItemCaseSensitive(root, "state");
    const cJSON *mode = cJSON_GetObjectItemCaseSensitive(root, "mode");
    const cJSON *aborted = cJSON_GetObjectItemCaseSensitive(root, "is_aborted");
    bool control_mode = cJSON_IsString(type) && strcmp(type->valuestring, "mode") == 0 &&
                        cJSON_IsString(mode) && strcmp(mode->valuestring, "control") == 0;
    bool tts_stopped = cJSON_IsString(type) && strcmp(type->valuestring, "tts") == 0 &&
                       cJSON_IsString(state) && strcmp(state->valuestring, "stop") == 0;
    bool room_mode = cJSON_IsString(type) && strcmp(type->valuestring, "mode") == 0 &&
                     cJSON_IsString(mode) && strcmp(mode->valuestring, "room") == 0;
    if (control_mode || (tts_stopped && cJSON_IsTrue(aborted))) {
        audio_clear_playback();
    } else if (tts_stopped) {
        audio_wait_for_playback_drain();
    }
    if (room_mode) {
        portENTER_CRITICAL(&s_playback_ack_mux);
        s_playback_ack_pending = false;
        portEXIT_CRITICAL(&s_playback_ack_mux);
    }
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
        send_device_info();
        send_pending_playback_ack();
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
                handle_control_event((const char *)s_rx_frame, s_rx_frame_size);
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
        .buffer_size = DEVICE_INFO_MAX_BYTES,
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

void backend_client_send_playback_drained(void)
{
    portENTER_CRITICAL(&s_playback_ack_mux);
    s_playback_ack_pending = true;
    portEXIT_CRITICAL(&s_playback_ack_mux);
    send_pending_playback_ack();
}
