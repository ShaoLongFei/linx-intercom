#include "provisioning.h"

#include <stdlib.h>
#include <string.h>
#include "blufi_security.h"
#include "device_config.h"
#include "backend_client.h"
#include "esp_blufi.h"
#include "esp_blufi_api.h"
#include "esp_bt.h"
#include "esp_bt_main.h"
#include "esp_check.h"
#include "esp_event.h"
#include "esp_gap_ble_api.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_wifi.h"
#include "esp_system.h"
#include "driver/gpio.h"
#include "sdkconfig.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "nvs_flash.h"

static const char *TAG = "provisioning";
static bool s_ble_connected;
static bool s_wifi_ready;
static bool s_blufi_started;
static uint8_t s_wifi_retries;
static wifi_config_t s_sta_config;

#if CONFIG_INTERCOM_RESET_BUTTON_GPIO >= 0
static void reset_button_task(void *arg)
{
    (void)arg;
    bool pressed = false;
    bool reset_sent = false;
    TickType_t pressed_at = 0;
    while (true) {
        bool down = gpio_get_level(CONFIG_INTERCOM_RESET_BUTTON_GPIO) == 0;
        if (down && !pressed) {
            pressed = true;
            pressed_at = xTaskGetTickCount();
            reset_sent = false;
        } else if (!down) {
            pressed = false;
            reset_sent = false;
        } else if (pressed && !reset_sent &&
                   xTaskGetTickCount() - pressed_at >= pdMS_TO_TICKS(3000)) {
            reset_sent = true;
            provisioning_reset_wifi();
        }
        vTaskDelay(pdMS_TO_TICKS(50));
    }
}
#endif

static void report_wifi_status(void)
{
    wifi_mode_t mode = WIFI_MODE_STA;
    wifi_ap_record_t ap = {0};
    esp_blufi_extra_info_t extra = {0};
    (void)esp_wifi_get_mode(&mode);
    esp_blufi_sta_conn_state_t state = ESP_BLUFI_STA_CONN_FAIL;
    if (s_wifi_ready && esp_wifi_sta_get_ap_info(&ap) == ESP_OK) {
        state = ESP_BLUFI_STA_CONN_SUCCESS;
        extra.sta_bssid_set = true;
        memcpy(extra.sta_bssid, ap.bssid, sizeof(extra.sta_bssid));
        extra.sta_ssid = ap.ssid;
        extra.sta_ssid_len = strnlen((const char *)ap.ssid, sizeof(ap.ssid));
    } else if (s_wifi_retries > 0 && s_wifi_retries < 5) {
        state = ESP_BLUFI_STA_CONNECTING;
        extra.sta_max_conn_retry_set = true;
        extra.sta_max_conn_retry = 5;
    }
    esp_err_t err = esp_blufi_send_wifi_conn_report(mode, state, 0, &extra);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "BluFi status report failed: %s", esp_err_to_name(err));
    }
}

static void start_blufi(void)
{
    if (s_wifi_ready) {
        return;
    }
    if (s_blufi_started) {
        esp_blufi_adv_start();
        ESP_LOGI(TAG, "BluFi advertising reopened for Wi-Fi recovery");
        return;
    }
    ESP_LOGI(TAG, "starting BluFi provisioning; BLE remains closed after Wi-Fi connects");
    esp_bt_controller_config_t bt_config = BT_CONTROLLER_INIT_CONFIG_DEFAULT();
    esp_err_t err = esp_bt_controller_init(&bt_config);
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
        ESP_LOGE(TAG, "Bluetooth controller initialization failed: %s", esp_err_to_name(err));
        return;
    }
    err = esp_bt_controller_enable(ESP_BT_MODE_BLE);
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
        ESP_LOGE(TAG, "Bluetooth controller enable failed: %s", esp_err_to_name(err));
        return;
    }
    err = esp_bluedroid_init();
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
        ESP_LOGE(TAG, "Bluedroid initialization failed: %s", esp_err_to_name(err));
        return;
    }
    err = esp_bluedroid_enable();
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
        ESP_LOGE(TAG, "Bluedroid enable failed: %s", esp_err_to_name(err));
        return;
    }

    extern void blufi_event_callback(esp_blufi_cb_event_t event, esp_blufi_cb_param_t *param);
    esp_blufi_callbacks_t callbacks = {
        .event_cb = blufi_event_callback,
        .negotiate_data_handler = blufi_dh_negotiate_data_handler,
        .encrypt_func = blufi_aes_encrypt,
        .decrypt_func = blufi_aes_decrypt,
        .checksum_func = blufi_crc_checksum,
    };
    err = esp_blufi_register_callbacks(&callbacks);
    if (err == ESP_OK) {
        err = esp_ble_gap_register_callback(esp_blufi_gap_event_handler);
    }
    if (err == ESP_OK) {
        err = esp_blufi_profile_init();
    }
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "BluFi profile initialization failed: %s", esp_err_to_name(err));
        return;
    }
    s_blufi_started = true;
}

static void on_wifi_event(void *arg, esp_event_base_t event_base, int32_t event_id, void *event_data)
{
    (void)arg;
    (void)event_base;
    if (event_id == WIFI_EVENT_STA_START) {
        memset(&s_sta_config, 0, sizeof(s_sta_config));
        esp_err_t err = esp_wifi_get_config(WIFI_IF_STA, &s_sta_config);
        if (err != ESP_OK || s_sta_config.sta.ssid[0] == '\0') {
            start_blufi();
        } else {
            // Wi-Fi 驱动从 NVS 读取凭据；只有空配置才打开首次 BluFi 配网。
            ESP_LOGI(TAG, "stored Wi-Fi credentials found; connecting without logging SSID or password");
            s_wifi_retries = 1;
            err = esp_wifi_connect();
            if (err != ESP_OK) {
                ESP_LOGE(TAG, "initial Wi-Fi connection request failed: %s", esp_err_to_name(err));
                start_blufi();
            }
        }
    } else if (event_id == WIFI_EVENT_STA_DISCONNECTED) {
        wifi_event_sta_disconnected_t *disconnected = event_data;
        s_wifi_ready = false;
        if (s_wifi_retries < 5) {
            s_wifi_retries++;
            ESP_LOGW(TAG, "Wi-Fi disconnected; reason=%d retry=%u/5", disconnected->reason,
                     (unsigned)s_wifi_retries);
            esp_err_t err = esp_wifi_connect();
            if (err != ESP_OK) {
                ESP_LOGW(TAG, "Wi-Fi reconnect request failed: %s", esp_err_to_name(err));
            }
        } else {
            ESP_LOGE(TAG, "Wi-Fi reconnect limit reached; reopening BluFi for credential recovery");
            start_blufi();
        }
        if (s_ble_connected) {
            report_wifi_status();
        }
    } else if (event_id == WIFI_EVENT_SCAN_DONE) {
        uint16_t count = 0;
        esp_err_t err = esp_wifi_scan_get_ap_num(&count);
        if (err != ESP_OK || count == 0) {
            ESP_LOGW(TAG, "Wi-Fi scan returned no access points; error=%s", esp_err_to_name(err));
            (void)esp_blufi_send_error_info(ESP_BLUFI_WIFI_SCAN_FAIL);
            return;
        }
        wifi_ap_record_t *records = calloc(count, sizeof(*records));
        esp_blufi_ap_record_t *list = calloc(count, sizeof(*list));
        if (records == NULL || list == NULL) {
            free(records);
            free(list);
            ESP_LOGE(TAG, "Wi-Fi scan result allocation failed; count=%u", (unsigned)count);
            (void)esp_blufi_send_error_info(ESP_BLUFI_WIFI_SCAN_FAIL);
            return;
        }
        uint16_t actual = count;
        err = esp_wifi_scan_get_ap_records(&actual, records);
        if (err == ESP_OK) {
            for (uint16_t i = 0; i < actual; i++) {
                memcpy(list[i].ssid, records[i].ssid, sizeof(list[i].ssid));
                list[i].rssi = records[i].rssi;
            }
            err = esp_blufi_send_wifi_list(actual, list);
        }
        ESP_LOGI(TAG, "BluFi Wi-Fi scan completed; networks=%u result=%s", (unsigned)actual,
                 esp_err_to_name(err));
        free(records);
        free(list);
    }
}

static void on_ip_event(void *arg, esp_event_base_t event_base, int32_t event_id, void *event_data)
{
    (void)arg;
    (void)event_base;
    (void)event_data;
    if (event_id != IP_EVENT_STA_GOT_IP) {
        return;
    }
    s_wifi_ready = true;
    s_wifi_retries = 0;
    ESP_LOGI(TAG, "Wi-Fi connected and IP acquired");
    if (s_ble_connected) {
        report_wifi_status();
    }
    if (s_blufi_started) {
        // 配网完成后停止广播并断开手机，避免长期暴露配网入口。
        esp_blufi_adv_stop();
        esp_blufi_disconnect();
        ESP_LOGI(TAG, "BluFi provisioning closed after successful Wi-Fi connection");
    }
    esp_err_t err = backend_client_start();
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "backend startup failed after Wi-Fi connection: %s", esp_err_to_name(err));
    }
}

void blufi_event_callback(esp_blufi_cb_event_t event, esp_blufi_cb_param_t *param)
{
    switch (event) {
    case ESP_BLUFI_EVENT_INIT_FINISH:
        ESP_LOGI(TAG, "BluFi profile initialized; advertising started");
        esp_blufi_adv_start();
        break;
    case ESP_BLUFI_EVENT_BLE_CONNECT:
        s_ble_connected = true;
        esp_blufi_adv_stop();
        ESP_LOGI(TAG, "phone connected to BluFi");
        if (blufi_security_init() != ESP_OK) {
            ESP_LOGE(TAG, "BluFi security context allocation failed");
            esp_blufi_disconnect();
        }
        report_wifi_status();
        break;
    case ESP_BLUFI_EVENT_BLE_DISCONNECT:
        s_ble_connected = false;
        blufi_security_deinit();
        ESP_LOGI(TAG, "phone disconnected from BluFi");
        if (!s_wifi_ready) {
            esp_blufi_adv_start();
        }
        break;
    case ESP_BLUFI_EVENT_SET_WIFI_OPMODE:
        ESP_LOGI(TAG, "BluFi requested Wi-Fi operation mode; enforcing station mode");
        (void)esp_wifi_set_mode(WIFI_MODE_STA);
        break;
    case ESP_BLUFI_EVENT_RECV_STA_SSID:
        if (param->sta_ssid.ssid_len == 0 || param->sta_ssid.ssid_len > sizeof(s_sta_config.sta.ssid)) {
            ESP_LOGW(TAG, "BluFi rejected invalid SSID length=%u", (unsigned)param->sta_ssid.ssid_len);
            break;
        }
        memset(s_sta_config.sta.ssid, 0, sizeof(s_sta_config.sta.ssid));
        memcpy(s_sta_config.sta.ssid, param->sta_ssid.ssid, param->sta_ssid.ssid_len);
        {
            esp_err_t err = esp_wifi_set_config(WIFI_IF_STA, &s_sta_config);
            if (err != ESP_OK) {
                ESP_LOGE(TAG, "store Wi-Fi network name failed: %s", esp_err_to_name(err));
                break;
            }
        }
        ESP_LOGI(TAG, "BluFi received Wi-Fi network name; bytes=%u", (unsigned)param->sta_ssid.ssid_len);
        break;
    case ESP_BLUFI_EVENT_RECV_STA_PASSWD:
        if (param->sta_passwd.passwd_len > sizeof(s_sta_config.sta.password)) {
            ESP_LOGW(TAG, "BluFi rejected invalid password length=%u",
                     (unsigned)param->sta_passwd.passwd_len);
            break;
        }
        memset(s_sta_config.sta.password, 0, sizeof(s_sta_config.sta.password));
        memcpy(s_sta_config.sta.password, param->sta_passwd.passwd, param->sta_passwd.passwd_len);
        // Wi-Fi 凭据由驱动写入 NVS；应用层不落盘、不记录明文，也不输出密码。
        {
            esp_err_t err = esp_wifi_set_config(WIFI_IF_STA, &s_sta_config);
            if (err != ESP_OK) {
                ESP_LOGE(TAG, "store Wi-Fi password failed: %s", esp_err_to_name(err));
                break;
            }
        }
        ESP_LOGI(TAG, "BluFi received Wi-Fi password; bytes=%u (value redacted)",
                 (unsigned)param->sta_passwd.passwd_len);
        break;
    case ESP_BLUFI_EVENT_RECV_STA_BSSID:
        memcpy(s_sta_config.sta.bssid, param->sta_bssid.bssid, sizeof(s_sta_config.sta.bssid));
        s_sta_config.sta.bssid_set = true;
        {
            esp_err_t err = esp_wifi_set_config(WIFI_IF_STA, &s_sta_config);
            if (err != ESP_OK) {
                ESP_LOGE(TAG, "store Wi-Fi BSSID failed: %s", esp_err_to_name(err));
                break;
            }
        }
        ESP_LOGI(TAG, "BluFi received access point BSSID");
        break;
    case ESP_BLUFI_EVENT_REQ_CONNECT_TO_AP:
        ESP_LOGI(TAG, "BluFi requested Wi-Fi connection");
        s_wifi_retries = 1;
        (void)esp_wifi_disconnect();
        {
            esp_err_t err = esp_wifi_connect();
            if (err != ESP_OK) {
                ESP_LOGE(TAG, "Wi-Fi connect request failed: %s", esp_err_to_name(err));
                break;
            }
        }
        report_wifi_status();
        break;
    case ESP_BLUFI_EVENT_REQ_DISCONNECT_FROM_AP:
        ESP_LOGI(TAG, "BluFi requested Wi-Fi disconnection");
        (void)esp_wifi_disconnect();
        break;
    case ESP_BLUFI_EVENT_GET_WIFI_LIST: {
        wifi_scan_config_t scan = {.show_hidden = false};
        esp_err_t err = esp_wifi_scan_start(&scan, false);
        if (err != ESP_OK) {
            ESP_LOGW(TAG, "start Wi-Fi scan failed: %s", esp_err_to_name(err));
            (void)esp_blufi_send_error_info(ESP_BLUFI_WIFI_SCAN_FAIL);
        } else {
            ESP_LOGI(TAG, "BluFi Wi-Fi scan requested");
        }
        break;
    }
    case ESP_BLUFI_EVENT_GET_WIFI_STATUS:
        report_wifi_status();
        break;
    case ESP_BLUFI_EVENT_REPORT_ERROR:
        ESP_LOGW(TAG, "BluFi peer reported protocol error; code=%d", param->report_error.state);
        // 这是手机端报告给设备的错误，只记录，避免把同一错误再次回发。
        break;
    case ESP_BLUFI_EVENT_RECV_SLAVE_DISCONNECT_BLE:
        ESP_LOGI(TAG, "phone requested BluFi disconnect");
        esp_blufi_disconnect();
        break;
    default:
        ESP_LOGD(TAG, "BluFi event ignored; event=%d", event);
        break;
    }
}

esp_err_t provisioning_init(void)
{
    ESP_RETURN_ON_ERROR(esp_netif_init(), TAG, "initialize network interface failed");
    ESP_RETURN_ON_ERROR(esp_event_loop_create_default(), TAG, "create default event loop failed");
    if (esp_netif_create_default_wifi_sta() == NULL) {
        return ESP_FAIL;
    }
    wifi_init_config_t config = WIFI_INIT_CONFIG_DEFAULT();
    ESP_RETURN_ON_ERROR(esp_wifi_init(&config), TAG, "initialize Wi-Fi driver failed");
    ESP_RETURN_ON_ERROR(esp_wifi_set_storage(WIFI_STORAGE_FLASH), TAG, "enable Wi-Fi NVS storage failed");
    ESP_RETURN_ON_ERROR(esp_wifi_set_mode(WIFI_MODE_STA), TAG, "set Wi-Fi station mode failed");
    ESP_RETURN_ON_ERROR(esp_event_handler_register(WIFI_EVENT, ESP_EVENT_ANY_ID, on_wifi_event, NULL), TAG,
                        "register Wi-Fi event handler failed");
    ESP_RETURN_ON_ERROR(esp_event_handler_register(IP_EVENT, IP_EVENT_STA_GOT_IP, on_ip_event, NULL), TAG,
                        "register IP event handler failed");
    ESP_RETURN_ON_ERROR(esp_wifi_start(), TAG, "start Wi-Fi failed");
#if CONFIG_INTERCOM_RESET_BUTTON_GPIO >= 0
    {
        gpio_config_t button_config = {
            .pin_bit_mask = 1ULL << CONFIG_INTERCOM_RESET_BUTTON_GPIO,
            .mode = GPIO_MODE_INPUT,
            .pull_up_en = GPIO_PULLUP_ENABLE,
            .pull_down_en = GPIO_PULLDOWN_DISABLE,
            .intr_type = GPIO_INTR_DISABLE,
        };
        ESP_RETURN_ON_ERROR(gpio_config(&button_config), TAG, "configure reset button failed");
        BaseType_t task_ok = xTaskCreate(reset_button_task, "wifi_reset_button", 2048, NULL, 3, NULL);
        ESP_RETURN_ON_FALSE(task_ok == pdPASS, ESP_ERR_NO_MEM, TAG, "start reset button task failed");
        ESP_LOGI(TAG, "hold reset button for three seconds to clear Wi-Fi credentials");
    }
#endif
    ESP_LOGI(TAG, "Wi-Fi initialized; stored credentials stay in the ESP-IDF Wi-Fi NVS partition");
    return ESP_OK;
}

void provisioning_reset_wifi(void)
{
    ESP_LOGW(TAG, "reset-to-provisioning requested; clearing stored Wi-Fi configuration");
    esp_err_t err = esp_wifi_restore();
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "clear stored Wi-Fi configuration failed: %s", esp_err_to_name(err));
        return;
    }
    esp_restart();
}
