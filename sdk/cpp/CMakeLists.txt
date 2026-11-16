cmake_minimum_required(VERSION 3.16)
project(capsulad_cpp_example LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

# Point this at the installed header directory (contains capsulad/capsulad.h).
set(CAPSULAD_INCLUDE_DIR
    "${CMAKE_CURRENT_SOURCE_DIR}/../../build/include"
    CACHE PATH "Directory containing capsulad/capsulad.h")

find_package(Threads REQUIRED)

add_executable(capsulad-example example.cpp)
target_include_directories(capsulad-example PRIVATE "${CAPSULAD_INCLUDE_DIR}")
# libcapsulad is loaded at runtime with dlopen; only the loader library is linked.
target_link_libraries(capsulad-example PRIVATE Threads::Threads ${CMAKE_DL_LIBS})
