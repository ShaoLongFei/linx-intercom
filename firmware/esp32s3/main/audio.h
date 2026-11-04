#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "esp_err.h"

typedef bool (*audio_send_frame_fn)(const uint8_t *pcm, size_t size);
typedef void (*audio_playback_drained_fn)(void);

esp_err_t audio_init(void);
esp_err_t audio_start(audio_send_frame_fn send_frame, audio_playback_drained_fn playback_drained);
void audio_play_frame(const uint8_t *pcm, size_t size);
void audio_wait_for_playback_drain(void);
void audio_clear_playback(void);
