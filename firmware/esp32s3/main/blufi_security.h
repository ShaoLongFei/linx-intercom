#pragma once

#include <stdbool.h>
#include <stdint.h>
#include "esp_err.h"

void blufi_dh_negotiate_data_handler(uint8_t *data, int len, uint8_t **output_data,
                                     int *output_len, bool *need_free);
int blufi_aes_encrypt(uint8_t iv8, uint8_t *data, int size);
int blufi_aes_decrypt(uint8_t iv8, uint8_t *data, int size);
uint16_t blufi_crc_checksum(uint8_t iv8, uint8_t *data, int size);
esp_err_t blufi_security_init(void);
void blufi_security_deinit(void);
