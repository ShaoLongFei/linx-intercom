#include "device_config.h"
#include "provisioning.h"
#include "audio.h"
#include "backend_client.h"
#include "esp_log.h"
#include "nvs_flash.h"
#include "esp_err.h"

static const char *TAG = "intercom";

void app_main(void)
{
    esp_err_t err = nvs_flash_init();
    if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_LOGW(TAG, "NVS needs migration; erasing stored configuration");
        ESP_ERROR_CHECK(nvs_flash_erase());
        err = nvs_flash_init();
    }
    ESP_ERROR_CHECK(err);
    ESP_ERROR_CHECK(device_config_init());
    ESP_ERROR_CHECK(audio_init());
    ESP_ERROR_CHECK(provisioning_init());
    ESP_ERROR_CHECK(audio_start(backend_client_send_audio));
    ESP_LOGI(TAG, "ESP32-S3 intercom initialized; waiting for Wi-Fi provisioning or reconnect");
}
