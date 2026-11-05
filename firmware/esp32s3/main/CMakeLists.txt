idf_component_register(
    SRCS "main.c" "device_config.c" "provisioning.c" "audio.c" "backend_client.c" "blufi_security.c"
    INCLUDE_DIRS "."
    REQUIRES app_update esp_app_format esp_partition esp_system bt driver esp_event esp_netif esp_wifi esp_websocket_client json nvs_flash spi_flash
    PRIV_REQUIRES esp_driver_gpio esp_driver_i2s esp_hw_support mbedtls esp-tls)
