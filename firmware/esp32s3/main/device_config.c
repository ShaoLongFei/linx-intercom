#include "device_config.h"

#include <stdio.h>
#include <string.h>
#include "esp_check.h"
#include "esp_mac.h"
#include "esp_log.h"
#include "sdkconfig.h"

static const char *TAG = "device_config";
static device_config_t s_config;

esp_err_t device_config_init(void)
{
    uint8_t mac[6];
    ESP_RETURN_ON_ERROR(esp_read_mac(mac, ESP_MAC_WIFI_STA), TAG, "read station MAC failed");
    // 服务端按注册的物理设备 ID 校验 token；固定取 station MAC 保证重启后身份不变。
    snprintf(s_config.device_id, sizeof(s_config.device_id), "%02X:%02X:%02X:%02X:%02X:%02X",
             mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
    // menuconfig 是外部输入，拒绝静默截断 token 或 URI，避免以错误身份/地址连接。
    if (strlen(CONFIG_INTERCOM_DEVICE_TOKEN) >= sizeof(s_config.device_token) ||
        strlen(CONFIG_INTERCOM_BACKEND_URI) >= sizeof(s_config.backend_uri)) {
        ESP_LOGE(TAG, "device token or backend URI exceeds supported configuration length");
        return ESP_ERR_INVALID_SIZE;
    }
    snprintf(s_config.device_token, sizeof(s_config.device_token), "%s", CONFIG_INTERCOM_DEVICE_TOKEN);
    snprintf(s_config.backend_uri, sizeof(s_config.backend_uri), "%s", CONFIG_INTERCOM_BACKEND_URI);
    if (s_config.device_token[0] == '\0') {
        ESP_LOGW(TAG, "device token is empty; configure it in menuconfig before connecting to the backend");
    }
    ESP_LOGI(TAG, "device identity loaded; device_id=%s", s_config.device_id);
    return ESP_OK;
}

const device_config_t *device_config_get(void)
{
    return &s_config;
}
