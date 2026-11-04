#pragma once

#include "esp_err.h"

typedef struct {
    char device_id[32];
    char device_token[160];
    char backend_uri[160];
} device_config_t;

esp_err_t device_config_init(void);
const device_config_t *device_config_get(void);
