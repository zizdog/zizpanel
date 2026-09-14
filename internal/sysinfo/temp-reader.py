#!/usr/bin/env python3
"""读取 Apple Silicon 的温度传感器原始值，输出 JSON 给面板。

为什么是 Python 而不是 Go 原生：
  macOS 上读 SoC 温度只有两条路 —— SMC（AppleSMC 用户客户端）或 IOHID
  （AppleVendor 页 0xff00 / usage 5 的温度传感器）。两者都要调 IOKit/CoreFoundation
  的 C API：Go 里要么用 cgo（会给发布流程引入交叉编译风险 —— 面板同时要出
  arm64 与 amd64 两个包），要么引第三方 FFI 库（本机连 proxy.golang.org 都不通）。
  系统自带的 /usr/bin/python3（Command Line Tools）用 ctypes 就能直接调，
  零新增依赖、零编译，面板把脚本从 stdin 喂给它即可。

为什么不用 powermetrics：
  Apple Silicon 上 powermetrics **没有 smc 采样器**（M4 实测只支持
  tasks/battery/network/disk/interrupts/cpu_power/thermal/sfi/gpu_power/ane_power），
  而 thermal 给的是"热压力等级"不是温度。老的 `--samplers smc` 写法在 M 系列上
  永远拿不到值 —— 面板因此一直显示"不可读取（需 root）"，那是**误报**。

输出（stdout，JSON）：
  {"ok": true, "sensors": [{"name": "PMU tdie6", "c": 36.29}, ...]}
  ok=false 时带 "error" 说明原因。

只输出原始传感器列表，**选哪个当"CPU 温度"由 Go 侧决定**（那样可以用单测锁住规则）。
"""
import ctypes
import json
import sys

IOKIT = "/System/Library/Frameworks/IOKit.framework/IOKit"
COREFOUNDATION = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"

kCFNumberSInt32Type = 3
kCFStringEncodingUTF8 = 0x08000100
kIOHIDEventTypeTemperature = 15
# IOHIDEventFieldBase(type) = type << 16，温度字段就是 15 << 16
kIOHIDEventFieldTemperatureLevel = kIOHIDEventTypeTemperature << 16
APPLE_VENDOR_PAGE = 0xFF00
TEMPERATURE_USAGE = 5


def load():
    ik = ctypes.CDLL(IOKIT)
    cf = ctypes.CDLL(COREFOUNDATION)
    ik.IOHIDEventSystemClientCreate.restype = ctypes.c_void_p
    ik.IOHIDEventSystemClientCreate.argtypes = [ctypes.c_void_p]
    ik.IOHIDEventSystemClientSetMatching.restype = ctypes.c_int
    ik.IOHIDEventSystemClientSetMatching.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
    ik.IOHIDEventSystemClientCopyServices.restype = ctypes.c_void_p
    ik.IOHIDEventSystemClientCopyServices.argtypes = [ctypes.c_void_p]
    ik.IOHIDServiceClientCopyEvent.restype = ctypes.c_void_p
    ik.IOHIDServiceClientCopyEvent.argtypes = [
        ctypes.c_void_p, ctypes.c_int64, ctypes.c_int32, ctypes.c_int64]
    ik.IOHIDEventGetFloatValue.restype = ctypes.c_double
    ik.IOHIDEventGetFloatValue.argtypes = [ctypes.c_void_p, ctypes.c_int32]
    ik.IOHIDServiceClientCopyProperty.restype = ctypes.c_void_p
    ik.IOHIDServiceClientCopyProperty.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
    cf.CFArrayGetCount.restype = ctypes.c_long
    cf.CFArrayGetCount.argtypes = [ctypes.c_void_p]
    cf.CFArrayGetValueAtIndex.restype = ctypes.c_void_p
    cf.CFArrayGetValueAtIndex.argtypes = [ctypes.c_void_p, ctypes.c_long]
    cf.CFDictionaryCreateMutable.restype = ctypes.c_void_p
    cf.CFDictionaryCreateMutable.argtypes = [
        ctypes.c_void_p, ctypes.c_long, ctypes.c_void_p, ctypes.c_void_p]
    cf.CFDictionarySetValue.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p]
    cf.CFNumberCreate.restype = ctypes.c_void_p
    cf.CFNumberCreate.argtypes = [ctypes.c_void_p, ctypes.c_int, ctypes.c_void_p]
    cf.CFStringCreateWithCString.restype = ctypes.c_void_p
    cf.CFStringCreateWithCString.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_uint32]
    cf.CFStringGetCString.restype = ctypes.c_bool
    cf.CFStringGetCString.argtypes = [
        ctypes.c_void_p, ctypes.c_char_p, ctypes.c_long, ctypes.c_uint32]
    return ik, cf


def main():
    try:
        ik, cf = load()
    except OSError as exc:
        print(json.dumps({"ok": False, "error": "无法加载 IOKit/CoreFoundation：%s" % exc}))
        return 1

    def cfstr(s):
        return cf.CFStringCreateWithCString(None, s.encode(), kCFStringEncodingUTF8)

    def cfint32(v):
        x = ctypes.c_int32(v)
        return cf.CFNumberCreate(None, kCFNumberSInt32Type, ctypes.byref(x))

    def cfstr_to_py(ref):
        if not ref:
            return None
        buf = ctypes.create_string_buffer(512)
        if cf.CFStringGetCString(ref, buf, len(buf), kCFStringEncodingUTF8):
            return buf.value.decode("utf-8", "replace")
        return None

    client = ik.IOHIDEventSystemClientCreate(None)
    if not client:
        print(json.dumps({"ok": False, "error": "IOHIDEventSystemClientCreate 失败"}))
        return 1

    matching = cf.CFDictionaryCreateMutable(None, 0, None, None)
    cf.CFDictionarySetValue(matching, cfstr("PrimaryUsagePage"), cfint32(APPLE_VENDOR_PAGE))
    cf.CFDictionarySetValue(matching, cfstr("PrimaryUsage"), cfint32(TEMPERATURE_USAGE))
    ik.IOHIDEventSystemClientSetMatching(client, matching)

    services = ik.IOHIDEventSystemClientCopyServices(client)
    if not services:
        print(json.dumps({"ok": False, "error": "拿不到 HID 温度传感器列表（可能需要 root）"}))
        return 1

    sensors = []
    for i in range(cf.CFArrayGetCount(services)):
        svc = cf.CFArrayGetValueAtIndex(services, i)
        event = ik.IOHIDServiceClientCopyEvent(svc, kIOHIDEventTypeTemperature, 0, 0)
        if not event:
            continue
        value = ik.IOHIDEventGetFloatValue(event, kIOHIDEventFieldTemperatureLevel)
        name = cfstr_to_py(ik.IOHIDServiceClientCopyProperty(svc, cfstr("Product")))
        sensors.append({"name": name or "", "c": float(value)})

    if not sensors:
        print(json.dumps({"ok": False, "error": "传感器列表为空"}))
        return 1
    print(json.dumps({"ok": True, "sensors": sensors}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
