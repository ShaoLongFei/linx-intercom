/* Adapted from the ESP-IDF v5.3.2 BluFi example (Unlicense / CC0-1.0). */
#include "blufi_security.h"

#include <stdlib.h>
#include <string.h>
#include "esp_crc.h"
#include "esp_random.h"
#include "esp_blufi_api.h"
#include "esp_log.h"
#include "mbedtls/aes.h"
#include "mbedtls/dhm.h"
#include "mbedtls/md5.h"

#define SEC_DH_PARAM_LEN 0x00
#define SEC_DH_PARAM_DATA 0x01
#define SEC_DH_BUFFER_SIZE 128

typedef struct {
    uint8_t public_key[SEC_DH_BUFFER_SIZE];
    uint8_t shared_key[SEC_DH_BUFFER_SIZE];
    size_t shared_size;
    uint8_t psk[16];
    uint8_t iv[16];
    uint8_t *dh_params;
    size_t dh_params_size;
    mbedtls_dhm_context dhm;
    mbedtls_aes_context aes;
} blufi_security_t;

static const char *TAG = "blufi_security";
static blufi_security_t *s_security;
extern void btc_blufi_report_error(esp_blufi_error_state_t state);

static int random_bytes(void *ctx, unsigned char *output, size_t size)
{
    (void)ctx;
    esp_fill_random(output, size);
    return 0;
}

void blufi_dh_negotiate_data_handler(uint8_t *data, int len, uint8_t **output_data,
                                     int *output_len, bool *need_free)
{
    if (output_data != NULL) {
        *output_data = NULL;
    }
    if (output_len != NULL) {
        *output_len = 0;
    }
    if (need_free != NULL) {
        *need_free = false;
    }
    if (s_security == NULL || data == NULL || len < 1 || output_data == NULL ||
        output_len == NULL || need_free == NULL) {
        ESP_LOGE(TAG, "invalid BluFi key negotiation state");
        btc_blufi_report_error(ESP_BLUFI_INIT_SECURITY_ERROR);
        return;
    }
    if (data[0] == SEC_DH_PARAM_LEN) {
        if (len < 3) {
            btc_blufi_report_error(ESP_BLUFI_DATA_FORMAT_ERROR);
            return;
        }
        size_t size = ((size_t)data[1] << 8) | data[2];
        if (size == 0 || size > 1024) {
            btc_blufi_report_error(ESP_BLUFI_DH_PARAM_ERROR);
            return;
        }
        free(s_security->dh_params);
        s_security->dh_params = malloc(size);
        s_security->dh_params_size = size;
        if (s_security->dh_params == NULL) {
            btc_blufi_report_error(ESP_BLUFI_DH_MALLOC_ERROR);
        }
        return;
    }
    if (data[0] != SEC_DH_PARAM_DATA || s_security->dh_params == NULL ||
        (size_t)(len - 1) < s_security->dh_params_size) {
        btc_blufi_report_error(ESP_BLUFI_DH_PARAM_ERROR);
        return;
    }

    memcpy(s_security->dh_params, data + 1, s_security->dh_params_size);
    unsigned char *cursor = s_security->dh_params;
    int ret = mbedtls_dhm_read_params(&s_security->dhm, &cursor,
                                      cursor + s_security->dh_params_size);
    free(s_security->dh_params);
    s_security->dh_params = NULL;
    if (ret != 0) {
        ESP_LOGE(TAG, "read DH parameters failed; code=%d", ret);
        btc_blufi_report_error(ESP_BLUFI_READ_PARAM_ERROR);
        return;
    }
    size_t public_size = (size_t)mbedtls_dhm_get_len(&s_security->dhm);
    // 对端控制 DH 模数；必须先核对目标缓冲区容量，再生成公钥和共享密钥。
    if (public_size == 0 || public_size > sizeof(s_security->public_key) ||
        public_size > sizeof(s_security->shared_key)) {
        ESP_LOGE(TAG, "BluFi DH key size exceeds supported limit; bytes=%u", (unsigned)public_size);
        btc_blufi_report_error(ESP_BLUFI_DH_PARAM_ERROR);
        return;
    }
    ret = mbedtls_dhm_make_public(&s_security->dhm, (int)public_size,
                                  s_security->public_key, public_size, random_bytes, NULL);
    if (ret == 0) {
        ret = mbedtls_dhm_calc_secret(&s_security->dhm, s_security->shared_key,
                                      sizeof(s_security->shared_key), &s_security->shared_size,
                                      random_bytes, NULL);
    }
    if (ret == 0) {
        ret = mbedtls_md5(s_security->shared_key, s_security->shared_size, s_security->psk);
    }
    if (ret != 0) {
        ESP_LOGE(TAG, "derive BluFi session key failed; code=%d", ret);
        btc_blufi_report_error(ESP_BLUFI_DH_PARAM_ERROR);
        return;
    }
    ret = mbedtls_aes_setkey_enc(&s_security->aes, s_security->psk, 128);
    if (ret != 0) {
        btc_blufi_report_error(ESP_BLUFI_INIT_SECURITY_ERROR);
        return;
    }
    *output_data = s_security->public_key;
    *output_len = (int)public_size;
    *need_free = false;
}

static int crypt(uint8_t iv8, uint8_t *data, int size, int mode)
{
    if (s_security == NULL || data == NULL || size < 0) {
        return -1;
    }
    uint8_t iv[sizeof(s_security->iv)];
    memcpy(iv, s_security->iv, sizeof(iv));
    iv[0] = iv8;
    size_t offset = 0;
    int ret = mbedtls_aes_crypt_cfb128(&s_security->aes, mode, (size_t)size,
                                       &offset, iv, data, data);
    memset(iv, 0, sizeof(iv));
    return ret == 0 ? size : -1;
}

int blufi_aes_encrypt(uint8_t iv8, uint8_t *data, int size)
{
    return crypt(iv8, data, size, MBEDTLS_AES_ENCRYPT);
}

int blufi_aes_decrypt(uint8_t iv8, uint8_t *data, int size)
{
    return crypt(iv8, data, size, MBEDTLS_AES_DECRYPT);
}

uint16_t blufi_crc_checksum(uint8_t iv8, uint8_t *data, int size)
{
    (void)iv8;
    return esp_crc16_be(0, data, size);
}

esp_err_t blufi_security_init(void)
{
    if (s_security != NULL) {
        return ESP_OK;
    }
    s_security = calloc(1, sizeof(*s_security));
    if (s_security == NULL) {
        return ESP_ERR_NO_MEM;
    }
    mbedtls_dhm_init(&s_security->dhm);
    mbedtls_aes_init(&s_security->aes);
    return ESP_OK;
}

void blufi_security_deinit(void)
{
    if (s_security == NULL) {
        return;
    }
    free(s_security->dh_params);
    mbedtls_dhm_free(&s_security->dhm);
    mbedtls_aes_free(&s_security->aes);
    memset(s_security, 0, sizeof(*s_security));
    free(s_security);
    s_security = NULL;
}
