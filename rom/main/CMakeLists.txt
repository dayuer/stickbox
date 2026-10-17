idf_component_register(
    SRCS "main.c" "cloud_client.c" "netlog.c" "lcd_ui.c" "font_cn_16.c"
         "provision.c" "dlna.c" "stream.c" "jband.c"
    INCLUDE_DIRS "."
    REQUIRES esp_wifi esp_event nvs_flash esp_netif esp_http_server esp_http_client
             mqtt json lwip esp_timer esp-tls driver
    EMBED_FILES "assets/page1.jpg" "assets/page2.jpg" "assets/page3.jpg"
)
