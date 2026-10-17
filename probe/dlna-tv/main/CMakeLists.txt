idf_component_register(
    SRCS "main.c" "stream.c" "dlna.c" "ctrl.c" "lcd.c" "font_cn_16.c" "jband.c" "clockface.c"
    INCLUDE_DIRS "."
    REQUIRES esp_wifi nvs_flash esp_netif esp_event esp_http_server esp_http_client lwip esp_timer
    EMBED_FILES "../assets/page1.jpg" "../assets/page2.jpg" "../assets/page3.jpg"
)
