/*
 * Meow Music 设备遥控 · ESP32 固件示例
 * ------------------------------------------------------------------
 * 作用：让音箱/开发板主动向服务器长轮询，取走手机端下发的指令并执行。
 *      不需要公网 IP、不需要端口映射、不需要 MQTT。
 *
 *   手机/Web ──POST /api/device/command──▶ 服务器内存队列
 *   ESP32    ──GET  /api/esp32/poll─────▶ 有指令立即返回，没有就挂 20 秒
 *   ESP32    ──POST /api/esp32/ack──────▶ 回报执行结果
 *
 * 依赖库（Arduino IDE → 库管理器）：
 *   - ArduinoJson（6.21.x 或 7.x，代码已做兼容）
 *   - ESP32 board package（WiFi / HTTPClient 自带）
 *
 * 用法：
 *   1. 改下面 4 个常量（WiFi、服务器地址、设备 token）。
 *   2. 烧录。串口 115200 会打印收到的每条指令。
 *   3. 打开 http://服务器:2233/remote 点按钮，串口应立即可见。
 *
 * 设备 token 从哪来？
 *   Web 绑定设备成功后，服务器 devices.json 里那台设备会带一个 token 字段；
 *   也可以用 /api/device/list 查看（需要登录）。别把它写进公开仓库。
 * ------------------------------------------------------------------
 */

#include <WiFi.h>
#include <HTTPClient.h>
#include <ArduinoJson.h>

// ==================== 需要你改的 4 个值 ====================
const char *WIFI_SSID    = "你的WiFi名";
const char *WIFI_PASS    = "你的WiFi密码";
const char *SERVER       = "http://<SERVER_IP>:2233";  // 不要以 / 结尾
const char *DEVICE_TOKEN = "绑定设备后拿到的 token";
// ==========================================================

// 长轮询等待秒数（服务器上限 55，建议 20~30）
static const int POLL_WAIT_SEC   = 20;
// HTTP 超时要比 wait 大，否则每轮都被客户端先掐断
static const int HTTP_TIMEOUT_MS = (POLL_WAIT_SEC + 15) * 1000;

// 本机 MAC，服务器会拿它和 token 对应的设备比对
static String g_mac = "";

// ------------------------------------------------------------------
// 设备动作：这里是你固件真正干活的地方。
// 工程里已有播放逻辑的，把 Serial 那行换成调用自己的函数即可。
// ------------------------------------------------------------------
void doPlay()               { Serial.println("[CMD] play"); }
void doPause()              { Serial.println("[CMD] pause"); }
void doResume()             { Serial.println("[CMD] resume"); }
void doStop()               { Serial.println("[CMD] stop"); }
void doNext()               { Serial.println("[CMD] next"); }
void doPrev()               { Serial.println("[CMD] prev"); }
void doVolume(int value)    { Serial.printf("[CMD] volume -> %d\n", value); }  // 0~100
void doSeek(int positionMs) { Serial.printf("[CMD] seek -> %d ms\n", positionMs); }
void doPlaySong(const String &song, const String &singer) {
  Serial.printf("[CMD] play_song: %s - %s\n", singer.c_str(), song.c_str());
}
void doPlayPlaylist(const String &name) {
  Serial.printf("[CMD] play_playlist: %s\n", name.c_str());
}
void doReloadPlaylist()     { Serial.println("[CMD] reload_playlist"); }
void doSetLoop(bool on)     { Serial.printf("[CMD] loop = %d\n", on); }
void doSetShuffle(bool on)  { Serial.printf("[CMD] shuffle = %d\n", on); }
void doReboot()             { Serial.println("[CMD] reboot"); delay(300); ESP.restart(); }

// ------------------------------------------------------------------
// 网络
// ------------------------------------------------------------------
static bool wifiEnsure() {
  if (WiFi.status() == WL_CONNECTED) return true;

  Serial.printf("[WiFi] 连接 %s ", WIFI_SSID);
  WiFi.mode(WIFI_STA);
  WiFi.begin(WIFI_SSID, WIFI_PASS);
  for (int i = 0; i < 40 && WiFi.status() != WL_CONNECTED; i++) {
    delay(500);
    Serial.print(".");
  }
  Serial.println();

  if (WiFi.status() == WL_CONNECTED) {
    Serial.printf("[WiFi] 已连接，IP=%s\n", WiFi.localIP().toString().c_str());
    return true;
  }
  Serial.println("[WiFi] 连接失败，5 秒后重试");
  return false;
}

// 统一的 HTTP 调用：method 为 "GET" 或 "POST"
static bool httpCall(const String &method, const String &url, const String &payload,
                     int &statusCode, String &respBody) {
  HTTPClient http;
  http.setTimeout(HTTP_TIMEOUT_MS);
  http.setReuse(false);

  if (!http.begin(url)) {
    statusCode = -1;
    return false;
  }
  http.addHeader("X-Device-Token", DEVICE_TOKEN);
  if (payload.length() > 0) {
    http.addHeader("Content-Type", "application/json");
  }

  statusCode = (method == "POST") ? http.POST(payload) : http.GET();
  if (statusCode > 0) {
    respBody = http.getString();
  }
  http.end();
  return statusCode > 0;
}

// 开机自检：token 对不对、绑的是哪个账号
static void verifyToken() {
  String url = String(SERVER) + "/api/esp32/verify";
  String body;
  int code = 0;
  if (!httpCall("GET", url, "", code, body)) {
    Serial.println("[Boot] 无法连接服务器，检查 SERVER / WiFi");
    return;
  }
  if (code != 200) {
    Serial.printf("[Boot] token 校验失败 HTTP %d: %s\n", code, body.c_str());
    return;
  }
  Serial.printf("[Boot] token 有效：%s\n", body.c_str());
}

// ------------------------------------------------------------------
// 指令执行
// ------------------------------------------------------------------
static bool executeCommand(const String &action, JsonObject params) {
  if (action == "play")    { doPlay();    return true; }
  if (action == "pause")   { doPause();   return true; }
  if (action == "resume")  { doResume();  return true; }
  if (action == "stop")    { doStop();    return true; }
  if (action == "next")    { doNext();    return true; }
  if (action == "prev")    { doPrev();    return true; }
  if (action == "reboot")  { doReboot();  return true; }

  if (action == "volume") {
    // 面板传的是 {"value": 0~100}
    int v = params["value"] | 50;
    if (v < 0) v = 0;
    if (v > 100) v = 100;
    doVolume(v);
    return true;
  }
  if (action == "seek") {
    int ms = params["position"] | 0;
    if (ms < 0) ms = 0;
    doSeek(ms);
    return true;
  }
  if (action == "play_song") {
    doPlaySong(params["song"].as<String>(), params["singer"].as<String>());
    return true;
  }
  if (action == "play_playlist") {
    doPlayPlaylist(params["name"].as<String>());
    return true;
  }
  if (action == "reload_playlist") { doReloadPlaylist(); return true; }
  if (action == "set_loop")    { doSetLoop(params["value"] | false);    return true; }
  if (action == "set_shuffle") { doSetShuffle(params["value"] | false); return true; }

  Serial.printf("[CMD] 未知指令: %s\n", action.c_str());
  return false;
}

// 回报执行结果（服务器只用于面板展示，失败也不影响播放）
static void sendAck(const String &id, bool ok, const String &err) {
  if (id.length() == 0) return;

  String payload = "{\"id\":\"" + id + "\",\"ok\":";
  payload += ok ? "true" : "false";
  if (!ok && err.length() > 0) {
    payload += ",\"error\":\"" + err + "\"";
  }
  payload += "}";

  String url = String(SERVER) + "/api/esp32/ack";
  String body;
  int code = 0;
  httpCall("POST", url, payload, code, body);
  if (code != 200) {
    Serial.printf("[Ack] 回报失败 HTTP %d\n", code);
  }
}

// ------------------------------------------------------------------
// 一轮长轮询
// ------------------------------------------------------------------
static void pollOnce() {
  String url = String(SERVER) + "/api/esp32/poll?wait=" + String(POLL_WAIT_SEC) +
               "&mac=" + g_mac;
  String body;
  int code = 0;

  if (!httpCall("GET", url, "", code, body)) {
    Serial.println("[Poll] 网络不可用，2 秒后重试");
    delay(2000);
    return;
  }
  if (code == 401 || code == 403) {
    Serial.printf("[Poll] 鉴权失败 HTTP %d：检查 DEVICE_TOKEN，以及 MAC 是否和绑定的一致\n", code);
    delay(10000);
    return;
  }
  if (code != 200) {
    Serial.printf("[Poll] HTTP %d\n", code);
    delay(5000);
    return;
  }

#if ARDUINOJSON_VERSION_MAJOR >= 7
  JsonDocument doc;
#else
  DynamicJsonDocument doc(8192);
#endif
  DeserializationError err = deserializeJson(doc, body);
  if (err) {
    Serial.printf("[Poll] JSON 解析失败: %s\n", err.c_str());
    return;
  }

  if (!doc["commands"].is<JsonArray>()) return;

  for (JsonObject c : doc["commands"].as<JsonArray>()) {
    String id     = c["id"].as<String>();
    String action = c["action"].as<String>();
    JsonObject params = c["params"].is<JsonObject>() ? c["params"].as<JsonObject>()
                                                     : JsonObject();

    Serial.printf("[Poll] 收到 %s (%s)\n", action.c_str(), id.c_str());
    bool ok = executeCommand(action, params);
    sendAck(id, ok, ok ? "" : "unsupported action");
  }
}

// ------------------------------------------------------------------
void setup() {
  Serial.begin(115200);
  delay(300);
  Serial.println();
  Serial.println("=== Meow Music 遥控固件示例 ===");

  if (!wifiEnsure()) {
    Serial.println("[Boot] WiFi 没连上，进 loop 后继续重试");
  }

  g_mac = WiFi.macAddress();      // 形如 AA:BB:CC:DD:EE:FF
  g_mac.toUpperCase();
  Serial.printf("[Boot] MAC=%s  服务器=%s\n", g_mac.c_str(), SERVER);

  verifyToken();
}

void loop() {
  if (!wifiEnsure()) {
    delay(5000);
    return;
  }
  pollOnce();
  delay(200);   // 有指令时立即再轮询，空转时服务器端会挂住 20 秒
}
