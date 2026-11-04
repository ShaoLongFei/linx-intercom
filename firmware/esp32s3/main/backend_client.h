#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "esp_err.h"

esp_err_t backend_client_start(void);
bool backend_client_send_audio(const uint8_t *pcm, size_t size);
