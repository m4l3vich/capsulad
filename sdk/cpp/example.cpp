// Capsulad C++ consumer example.
//
// This is intentionally an example, not a second SDK: it uses the installed
// capsulad/capsulad.h and resolves libcapsulad dynamically with dlopen, exactly like a
// native app that was not linked against the shared engine. RAII guards close
// clients, calls and buses; buffers are always returned through buf_free.
//
// Build (canonical installed header is build/include/capsulad/capsulad.h):
//   c++ -std=c++17 -pthread -I build/include sdk/cpp/example.cpp -o build/capsulad-cpp-example -ldl
//
// Run:
//   export CAPSULAD_LIBRARY=$PWD/build/lib/libcapsulad.so.1
//   export CAPSULAD_SOCKET=/run/capsulad/hal.sock
//   export CAPSULAD_RUN_ID=run-...
//   ./build/capsulad-cpp-example capsulad.framework.v1.DeviceService GetInfo '{}'
//
// Set CAPSULAD_BUS=1 to open the AppBus, print events, echo routed calls and
// wait for disconnect. Set CAPSULAD_STREAM=1 for a server-streaming call.

#include <capsulad/capsulad.h>

#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <dlfcn.h>
#include <iostream>
#include <mutex>
#include <optional>
#include <stdexcept>
#include <string>
#include <string_view>
#include <utility>

namespace {

class CapsuleError : public std::runtime_error {
 public:
  CapsuleError(int32_t status, const std::string& message)
      : std::runtime_error(message), status_(status) {}
  int32_t status() const { return status_; }

 private:
  int32_t status_;
};

[[noreturn]] void fail(const std::string& message) {
  throw std::runtime_error("capsulad: " + message);
}

void check(const capsulad_api* api, int32_t status, const char* what) {
  if (status == CAPSULAD_STATUS_OK) return;
  const char* detail = api->last_error ? api->last_error() : nullptr;
  std::string message = std::string(what) + ": status " + std::to_string(status);
  if (detail != nullptr && *detail != '\0') {
    message += ": ";
    message += detail;
  }
  throw CapsuleError(status, message);
}

std::string env_or(const char* name, const char* fallback) {
  const char* value = std::getenv(name);
  return (value != nullptr && *value != '\0') ? std::string(value)
                                              : std::string(fallback);
}

const char* default_soname() {
#if defined(__APPLE__)
  return "libcapsulad.1.dylib";
#else
  return "libcapsulad.so.1";
#endif
}

std::string default_library_path() {
  const std::string explicit_path = env_or("CAPSULAD_LIBRARY", "");
  if (!explicit_path.empty()) return explicit_path;
  const std::string dir = env_or("CAPSULAD_LIBRARY_DIR", "");
  if (!dir.empty()) return dir + "/" + default_soname();
  return default_soname();
}

// Owns the dlopen handle and the copied ABI table.
class Library {
 public:
  Library() = default;
  Library(const Library&) = delete;
  Library& operator=(const Library&) = delete;
  Library(Library&& other) noexcept : handle_(other.handle_), api_(other.api_) {
    other.handle_ = nullptr;
  }
  Library& operator=(Library&& other) noexcept {
    if (this != &other) {
      // The engine's process-global runtime threads outlive all handles, so
      // a successfully loaded library is never dlclose'd.
      handle_ = other.handle_;
      api_ = other.api_;
      other.handle_ = nullptr;
    }
    return *this;
  }
  // Process-lifetime residency: never dlclose an initialized engine.
  ~Library() = default;

  const capsulad_api* api() const { return &api_; }

  static Library load() {
    const std::string path = default_library_path();
    Library library;
    library.handle_ = dlopen(path.c_str(), RTLD_NOW | RTLD_LOCAL);
    if (library.handle_ == nullptr) {
      const char* error = dlerror();
      fail("dlopen " + path + ": " + (error != nullptr ? error : "unknown"));
    }

    using GetApiFn = int32_t (*)(uint32_t, size_t, capsulad_api*);
    auto get_api =
        reinterpret_cast<GetApiFn>(dlsym(library.handle_, "capsulad_get_api"));
    if (get_api == nullptr) {
      fail(path + " does not export capsulad_get_api");
    }
    std::memset(&library.api_, 0, sizeof(library.api_));
    const int32_t status =
        get_api(CAPSULAD_ABI_MAJOR, sizeof(library.api_), &library.api_);
    if (status != CAPSULAD_STATUS_OK) {
      fail(path + " rejected ABI v1: status " + std::to_string(status));
    }
    if (library.api_.abi_major != CAPSULAD_ABI_MAJOR ||
        library.api_.struct_size < sizeof(capsulad_api)) {
      fail(path + " returned an incompatible ABI table");
    }
    require(library.api_);
    return library;
  }

 private:
  static void require(const capsulad_api& api) {
    if (api.client_connect == nullptr || api.client_close == nullptr ||
        api.client_unary == nullptr || api.call_start == nullptr ||
        api.call_send_json == nullptr || api.call_send_proto == nullptr ||
        api.call_half_close == nullptr || api.call_recv_json == nullptr ||
        api.call_recv_proto == nullptr || api.call_end == nullptr ||
        api.call_cancel == nullptr || api.call_retain == nullptr ||
        api.call_release == nullptr || api.call_status == nullptr ||
        api.bus_open == nullptr || api.bus_close == nullptr ||
        api.bus_ready == nullptr || api.buf_free == nullptr ||
        api.last_error == nullptr) {
      fail("ABI table is missing a required function");
    }
  }

  void* handle_ = nullptr;
  capsulad_api api_{};
};

// Copies and releases one native buffer.
std::string take_buf(const capsulad_api* api, capsulad_buf buf) {
  std::string out;
  if (buf.data != nullptr && buf.len != 0) {
    out.assign(reinterpret_cast<const char*>(buf.data), buf.len);
  }
  api->buf_free(buf);
  return out;
}

struct BufGuard {
  const capsulad_api* api;
  capsulad_buf buf{};
  ~BufGuard() {
    if (api != nullptr) api->buf_free(buf);
  }
};

class Call {
 public:
  Call(const capsulad_api* api, capsulad_handle handle, bool owns_reference)
      : api_(api), handle_(handle), owns_reference_(owns_reference) {}
  Call(const Call&) = delete;
  Call& operator=(const Call&) = delete;
  Call(Call&& other) noexcept
      : api_(other.api_),
        handle_(other.handle_),
        owns_reference_(other.owns_reference_) {
    other.handle_ = 0;
  }
  Call& operator=(Call&& other) noexcept {
    if (this != &other) {
      release();
      api_ = other.api_;
      handle_ = other.handle_;
      owns_reference_ = other.owns_reference_;
      other.handle_ = 0;
    }
    return *this;
  }
  ~Call() { release(); }

  capsulad_handle handle() const { return handle_; }

  Call retain() const {
    check(api_, api_->call_retain(handle_), "call_retain");
    return Call(api_, handle_, true);
  }

  void send_json(std::string_view json) const {
    check(api_,
          api_->call_send_json(
              handle_, reinterpret_cast<const uint8_t*>(json.data()), json.size()),
          "call_send_json");
  }

  void send_proto(std::string_view proto) const {
    check(api_,
          api_->call_send_proto(
              handle_, reinterpret_cast<const uint8_t*>(proto.data()), proto.size()),
          "call_send_proto");
  }

  void half_close() const {
    check(api_, api_->call_half_close(handle_), "call_half_close");
  }

  std::optional<std::string> recv_json(uint32_t timeout_ms) const {
    return recv(api_->call_recv_json, timeout_ms, "call_recv_json");
  }

  std::optional<std::string> recv_proto(uint32_t timeout_ms) const {
    return recv(api_->call_recv_proto, timeout_ms, "call_recv_proto");
  }

  void end(int32_t grpc_code, std::string_view detail) const {
    check(api_,
          api_->call_end(handle_, grpc_code, detail.data(), detail.size()),
          "call_end");
  }

  void cancel() const {
    check(api_, api_->call_cancel(handle_), "call_cancel");
  }

 private:
  using RecvFn = int32_t (*)(capsulad_handle, uint32_t, capsulad_buf*);

  std::optional<std::string> recv(RecvFn fn, uint32_t timeout_ms,
                                  const char* what) const {
    capsulad_buf out{};
    const int32_t status = fn(handle_, timeout_ms, &out);
    if (status == CAPSULAD_STATUS_OK) return take_buf(api_, out);
    if (status == CAPSULAD_STATUS_END_OF_STREAM) {
      api_->buf_free(out);
      return std::nullopt;
    }
    BufGuard guard{api_, out};
    if (status == CAPSULAD_STATUS_GRPC) {
      throw CapsuleError(status, std::string(what) + ": " + grpc_detail());
    }
    check(api_, status, what);
    return std::nullopt;
  }

  std::string grpc_detail() const {
    int32_t code = -1;
    capsulad_buf detail{};
    const int32_t status = api_->call_status(handle_, &code, &detail);
    const std::string text = take_buf(api_, detail);
    if (status != CAPSULAD_STATUS_OK) return "gRPC " + std::to_string(code);
    return "gRPC " + std::to_string(code) + ": " + text;
  }

  void release() {
    if (api_ != nullptr && handle_ != 0 && owns_reference_) {
      api_->call_release(handle_);
    }
    handle_ = 0;
  }

  const capsulad_api* api_ = nullptr;
  capsulad_handle handle_ = 0;
  bool owns_reference_ = true;
};

class Client {
 public:
  Client() = default;
  Client(const capsulad_api* api, capsulad_handle handle)
      : api_(api), handle_(handle) {}
  Client(const Client&) = delete;
  Client& operator=(const Client&) = delete;
  Client(Client&& other) noexcept : api_(other.api_), handle_(other.handle_) {
    other.handle_ = 0;
  }
  Client& operator=(Client&& other) noexcept {
    if (this != &other) {
      close();
      api_ = other.api_;
      handle_ = other.handle_;
      other.handle_ = 0;
    }
    return *this;
  }
  ~Client() { close(); }

  static Client connect(const capsulad_api* api, const std::string& socket,
                        const std::string& run_id, const std::string& app_id,
                        uint32_t timeout_ms) {
    capsulad_handle handle = 0;
    const int32_t status = api->client_connect(
        socket.data(), socket.size(), run_id.data(), run_id.size(),
        app_id.data(), app_id.size(), timeout_ms, &handle);
    check(api, status, "client_connect");
    if (handle == 0) fail("client_connect returned a zero handle");
    return Client(api, handle);
  }

  capsulad_handle handle() const { return handle_; }

  std::string unary_json(const std::string& service, const std::string& method,
                         std::string_view request, uint32_t timeout_ms) const {
    capsulad_buf out{};
    const int32_t status = api_->client_unary(
        handle_, service.data(), service.size(), method.data(), method.size(),
        reinterpret_cast<const uint8_t*>(request.data()), request.size(),
        timeout_ms, &out);
    if (status != CAPSULAD_STATUS_OK) {
      BufGuard guard{api_, out};
      check(api_, status, "client_unary");
    }
    return take_buf(api_, out);
  }

  Call start_call(const std::string& service, const std::string& method,
                  int32_t kind, uint32_t timeout_ms) const {
    capsulad_handle handle = 0;
    const int32_t status = api_->call_start(
        handle_, service.data(), service.size(), method.data(), method.size(),
        kind, timeout_ms, &handle);
    check(api_, status, "call_start");
    if (handle == 0) fail("call_start returned a zero handle");
    return Call(api_, handle, true);
  }

  Call start_routed(const std::string& target_app_id,
                    const std::string& service, const std::string& method,
                    uint32_t timeout_ms) const {
    capsulad_handle handle = 0;
    const int32_t status = api_->call_start_routed(
        handle_, target_app_id.data(), target_app_id.size(), service.data(),
        service.size(), method.data(), method.size(), timeout_ms, &handle);
    check(api_, status, "call_start_routed");
    if (handle == 0) fail("call_start_routed returned a zero handle");
    return Call(api_, handle, true);
  }

  class Bus;
  Bus open_bus(const capsulad_bus_callbacks_v1* callbacks) const;

 private:
  void close() {
    if (api_ != nullptr && handle_ != 0) api_->client_close(handle_);
    handle_ = 0;
  }

  const capsulad_api* api_ = nullptr;
  capsulad_handle handle_ = 0;
};

struct BusContext {
  const capsulad_api* api = nullptr;
  std::mutex mutex;
  std::condition_variable cv;
  bool disconnected = false;
};

void on_ready(capsulad_handle, void*) {
  try {
    std::cerr << "bus: ready\n";
  } catch (...) {
  }
}

void on_disconnected(capsulad_handle, int32_t status, const char* detail,
                     void* user_data) {
  try {
    auto* context = static_cast<BusContext*>(user_data);
    {
      std::lock_guard<std::mutex> lock(context->mutex);
      context->disconnected = true;
    }
    std::cerr << "bus: disconnected status=" << status
              << " detail=" << (detail != nullptr ? detail : "") << "\n";
    context->cv.notify_all();
  } catch (...) {
  }
}

void on_event(capsulad_handle, int32_t kind, const uint8_t* json, size_t len,
              void*) {
  try {
    const std::string payload =
        (json != nullptr && len != 0)
            ? std::string(reinterpret_cast<const char*>(json), len)
            : std::string();
    std::cerr << "bus: event kind=" << kind << " json=" << payload << "\n";
  } catch (...) {
  }
}

void on_call(capsulad_handle, capsulad_handle handle, const uint8_t* info,
             size_t info_len, void* user_data) {
  auto* context = static_cast<BusContext*>(user_data);
  // Inbound callback reference is owned by the library: do not release it.
  Call call(context->api, handle, false);
  try {
    const std::string info_text =
        (info != nullptr && info_len != 0)
            ? std::string(reinterpret_cast<const char*>(info), info_len)
            : std::string();
    std::cerr << "bus: call " << info_text << "\n";
    std::string request;
    for (;;) {
      capsulad_buf chunk{};
      const int32_t status =
          context->api->call_recv_proto(handle, 5000, &chunk);
      if (status == CAPSULAD_STATUS_OK) {
        if (chunk.data != nullptr && chunk.len != 0) {
          request.append(reinterpret_cast<const char*>(chunk.data), chunk.len);
        }
        context->api->buf_free(chunk);
        continue;
      }
      context->api->buf_free(chunk);
      if (status == CAPSULAD_STATUS_END_OF_STREAM) break;
      const char* last = context->api->last_error();
      const std::string detail =
          last != nullptr ? std::string(last) : std::string("request read failed");
      call.end(13, std::string_view(detail.data(), detail.size()));
      return;
    }
    const int32_t status = context->api->call_send_proto(
        handle, reinterpret_cast<const uint8_t*>(request.data()),
        request.size());
    if (status != CAPSULAD_STATUS_OK) {
      const char* last = context->api->last_error();
      const std::string detail =
          last != nullptr ? std::string(last) : std::string("reply failed");
      call.end(13, std::string_view(detail.data(), detail.size()));
      return;
    }
    call.end(0, "");
  } catch (const std::exception& error) {
    try {
      call.end(13, error.what());
    } catch (...) {
    }
  } catch (...) {
    try {
      call.end(13, "exception in on_call");
    } catch (...) {
    }
  }
}

void on_log(int32_t level, const char* message, void*) {
  try {
    std::cerr << "bus: log level=" << level << " " << (message ? message : "")
              << "\n";
  } catch (...) {
  }
}

class Client::Bus {
 public:
  Bus(const capsulad_api* api, capsulad_handle handle)
      : api_(api), handle_(handle) {}
  Bus(const Bus&) = delete;
  Bus& operator=(const Bus&) = delete;
  Bus(Bus&& other) noexcept : api_(other.api_), handle_(other.handle_) {
    other.handle_ = 0;
  }
  Bus& operator=(Bus&& other) noexcept {
    if (this != &other) {
      close();
      api_ = other.api_;
      handle_ = other.handle_;
      other.handle_ = 0;
    }
    return *this;
  }
  ~Bus() {
    if (api_ != nullptr && handle_ != 0) api_->bus_close(handle_);
  }

  void ready(uint32_t timeout_ms) {
    check(api_, api_->bus_ready(handle_, timeout_ms), "bus_ready");
  }

  void notify_invocation(const std::string& invocation_id, bool accepted,
                         const std::string& detail) {
    check(api_,
          api_->bus_notify_invocation(handle_, invocation_id.data(),
                                      invocation_id.size(), accepted ? 1 : 0,
                                      detail.data(), detail.size()),
          "bus_notify_invocation");
  }

  void notify_action(const std::string& action_id, bool succeeded,
                     const std::string& detail) {
    check(api_,
          api_->bus_notify_action(handle_, action_id.data(), action_id.size(),
                                  succeeded ? 1 : 0, detail.data(),
                                  detail.size()),
          "bus_notify_action");
  }

  void close() {
    if (api_ != nullptr && handle_ != 0) {
      const int32_t status = api_->bus_close(handle_);
      if (status == CAPSULAD_STATUS_OK || status == CAPSULAD_STATUS_CLOSED) {
        handle_ = 0;
        return;
      }
      check(api_, status, "bus_close");
    }
  }

 private:
  const capsulad_api* api_ = nullptr;
  capsulad_handle handle_ = 0;
};

Client::Bus Client::open_bus(
    const capsulad_bus_callbacks_v1* callbacks) const {
  capsulad_handle handle = 0;
  const int32_t status = api_->bus_open(handle_, callbacks, &handle);
  check(api_, status, "bus_open");
  if (handle == 0) fail("bus_open returned a zero handle");
  return Bus(api_, handle);
}

}  // namespace

int main(int argc, char** argv) {
  try {
    Library library = Library::load();
    const capsulad_api* api = library.api();
    const std::string socket = env_or("CAPSULAD_SOCKET", CAPSULAD_DEFAULT_SOCKET);
    const std::string run_id = env_or("CAPSULAD_RUN_ID", "");
    if (run_id.empty()) fail("CAPSULAD_RUN_ID is required");
    const std::string app_id = env_or("CAPSULAD_APP_ID", "");
    Client client = Client::connect(api, socket, run_id, app_id, 0);

    if (env_or("CAPSULAD_BUS", "0") == "1") {
      BusContext context{};
      context.api = api;
      capsulad_bus_callbacks_v1 callbacks{};
      callbacks.struct_size = sizeof(callbacks);
      callbacks.user_data = &context;
      callbacks.on_ready = &on_ready;
      callbacks.on_disconnected = &on_disconnected;
      callbacks.on_event = &on_event;
      callbacks.on_call = &on_call;
      callbacks.on_log = &on_log;
      Client::Bus bus = client.open_bus(&callbacks);
      bus.ready(5000);
      std::unique_lock<std::mutex> lock(context.mutex);
      context.cv.wait_for(lock, std::chrono::seconds(30),
                          [&context] { return context.disconnected; });
      bus.close();
      return 0;
    }

    const std::string service =
        argc > 1 ? std::string(argv[1])
                 : env_or("CAPSULAD_SERVICE",
                          "capsulad.framework.v1.DeviceService");
    const std::string method =
        argc > 2 ? std::string(argv[2]) : env_or("CAPSULAD_METHOD", "GetInfo");
    const std::string request =
        argc > 3 ? std::string(argv[3]) : env_or("CAPSULAD_REQUEST", "{}");

    if (env_or("CAPSULAD_STREAM", "0") == "1") {
      Call call = client.start_call(service, method,
                                    CAPSULAD_CALL_SERVER_STREAM, 0);
      call.send_json(request);
      call.half_close();
      while (auto message = call.recv_json(30000)) {
        std::cout << *message << "\n";
      }
    } else {
      std::cout << client.unary_json(service, method, request, 0) << "\n";
    }
    return 0;
  } catch (const std::exception& error) {
    std::cerr << "capsulad: " << error.what() << "\n";
    return 1;
  }
}
