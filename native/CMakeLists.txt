cmake_minimum_required(VERSION 3.16)
project(freeiran_native LANGUAGES CXX VERSION 0.2.0)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)

add_library(freeiran_native STATIC src/freeiran.cpp)
target_include_directories(freeiran_native PUBLIC include)
target_compile_options(freeiran_native PRIVATE
    $<$<CXX_COMPILER_ID:GNU,Clang>:-Wall -Wextra -Wpedantic -fno-exceptions>
    $<$<CXX_COMPILER_ID:MSVC>:/W4 /EHsc>
)

add_executable(test_native tests/test_native.cpp)
target_link_libraries(test_native PRIVATE freeiran_native)

enable_testing()
add_test(NAME native COMMAND test_native)
