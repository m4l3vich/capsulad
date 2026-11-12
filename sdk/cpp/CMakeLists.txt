cmake_minimum_required(VERSION 3.16)
project(capsula_cpp_example LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

# Point this at the installed header directory (contains capsula/capsula.h).
set(CAPSULA_INCLUDE_DIR
    "${CMAKE_CURRENT_SOURCE_DIR}/../../build/include"
    CACHE PATH "Directory containing capsula/capsula.h")

find_package(Threads REQUIRED)

add_executable(capsula-example example.cpp)
target_include_directories(capsula-example PRIVATE "${CAPSULA_INCLUDE_DIR}")
# libcapsula is loaded at runtime with dlopen; only the loader library is linked.
target_link_libraries(capsula-example PRIVATE Threads::Threads ${CMAKE_DL_LIBS})
