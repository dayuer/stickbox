idf_component_register(
    SRCS "main.c" "vdisk.c" "usbdev.c" "httpd.c"
    INCLUDE_DIRS "."
    REQUIRES driver esp_timer esp_wifi nvs_flash esp_netif esp_http_server
    EMBED_FILES "../assets/slide1.jpg" "../assets/slide2.jpg" "../assets/slide3.jpg"
)

# tinyusb 需要在包含路径上找到我们这份 tusb_config.h。
# esp_tinyusb 内部就是这么把自己的配置塞给 tinyusb 的；我们绕过它，所以得自己来。
idf_component_get_property(tusb_lib espressif__tinyusb COMPONENT_LIB)
target_include_directories(${tusb_lib} PRIVATE "${CMAKE_CURRENT_LIST_DIR}")
