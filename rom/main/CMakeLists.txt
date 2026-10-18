idf_component_register(
    SRCS "main.c" "cloud_client.c" "netlog.c" "lcd_ui.c" "font_cn_16.c"
         "provision.c" "dlna.c" "stream.c" "jband.c" "pairband.c"
         "ota.c" "coredump.c" "memdiag.c"
         "framepack.c" "framepack_band.c" "framepack_meta.c" "framepack_ver.c"
         "statusband.c" "statusmask.c" "framefetch.c"
         "photosha.c" "photostore_core.c" "photo_sd.c"
         "photo_json.c" "photo_policy.c" "photo_io.c" "photo_sync.c"
         "photo_play_seq.c" "photo_play.c"
    INCLUDE_DIRS "."
    REQUIRES esp_wifi esp_event nvs_flash esp_netif esp_http_server esp_http_client
             mqtt json lwip esp_timer esp-tls driver sdmmc
             app_update esp_https_ota espcoredump esp_partition
    EMBED_FILES "assets/blank.jpg"
)
