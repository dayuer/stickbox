# Distributed under the OSI-approved BSD 3-Clause License.  See accompanying
# file LICENSE.rst or https://cmake.org/licensing for details.

cmake_minimum_required(VERSION ${CMAKE_VERSION}) # this file comes with cmake

# If CMAKE_DISABLE_SOURCE_CHANGES is set to true and the source directory is an
# existing directory in our source tree, calling file(MAKE_DIRECTORY) on it
# would cause a fatal error, even though it would be a no-op.
if(NOT EXISTS "/Users/liyuqing/esp/esp-idf-v5.5/components/bootloader/subproject")
  file(MAKE_DIRECTORY "/Users/liyuqing/esp/esp-idf-v5.5/components/bootloader/subproject")
endif()
file(MAKE_DIRECTORY
  "/Users/liyuqing/sproot/esp-airprint-M0/rom/ build-m0a/bootloader"
  "/Users/liyuqing/sproot/esp-airprint-M0/rom/ build-m0a/bootloader-prefix"
  "/Users/liyuqing/sproot/esp-airprint-M0/rom/ build-m0a/bootloader-prefix/tmp"
  "/Users/liyuqing/sproot/esp-airprint-M0/rom/ build-m0a/bootloader-prefix/src/bootloader-stamp"
  "/Users/liyuqing/sproot/esp-airprint-M0/rom/ build-m0a/bootloader-prefix/src"
  "/Users/liyuqing/sproot/esp-airprint-M0/rom/ build-m0a/bootloader-prefix/src/bootloader-stamp"
)

set(configSubDirs )
foreach(subDir IN LISTS configSubDirs)
    file(MAKE_DIRECTORY "/Users/liyuqing/sproot/esp-airprint-M0/rom/ build-m0a/bootloader-prefix/src/bootloader-stamp/${subDir}")
endforeach()
if(cfgdir)
  file(MAKE_DIRECTORY "/Users/liyuqing/sproot/esp-airprint-M0/rom/ build-m0a/bootloader-prefix/src/bootloader-stamp${cfgdir}") # cfgdir has leading slash
endif()
