// Package i18n localizes the handful of user-facing strings the Go side
// produces (notification bodies, menu-bar style labels). The web UI carries
// its own dictionary (internal/webui/assets/i18n.js); this package only covers
// text that never passes through the webview.
//
// The default locale is English and nothing is auto-detected at import time,
// so library consumers (and their tests) are deterministic. The GUI app calls
// SetLocale(Detect()) once at startup to follow the macOS system language.
package i18n

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

var (
	mu     sync.RWMutex
	locale = "en"
)

// SetLocale switches the active message table. Unsupported languages fall back
// to English.
func SetLocale(lang string) {
	l := normalize(lang)
	if _, ok := messages[l]; !ok {
		l = "en"
	}
	mu.Lock()
	locale = l
	mu.Unlock()
}

// Locale returns the active language code (e.g. "en", "ko").
func Locale() string {
	mu.RLock()
	defer mu.RUnlock()
	return locale
}

// T returns the message for key in the active locale, formatted with args when
// given. Unknown keys fall back to English, then to the key itself.
func T(key string, args ...any) string {
	mu.RLock()
	l := locale
	mu.RUnlock()
	s, ok := messages[l][key]
	if !ok {
		s, ok = messages["en"][key]
	}
	if !ok {
		return key
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

// Detect returns the user's preferred language. On macOS it asks the global
// preferences (AppleLanguages) — the same source the system UI language uses —
// because a GUI app launched from Finder/launchd inherits no LANG. Elsewhere
// (and as a fallback) it reads the usual POSIX env vars. NETSCOPE_LANG
// overrides everything, which is also the escape hatch for testing a language.
func Detect() string {
	if l := os.Getenv("NETSCOPE_LANG"); l != "" {
		return normalize(l)
	}
	if runtime.GOOS == "darwin" {
		// Output is a plist array like: (\n    "ko-KR",\n    "en-US"\n)
		if out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output(); err == nil {
			if m := regexp.MustCompile(`[A-Za-z]{2,3}(?:[-_][A-Za-z0-9]+)*`).FindString(string(out)); m != "" {
				return normalize(m)
			}
		}
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" && v != "C" && v != "POSIX" {
			return normalize(v)
		}
	}
	return "en"
}

// normalize reduces a locale tag ("ko-KR", "en_US.UTF-8") to its base
// lowercase language code ("ko", "en").
func normalize(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	for _, sep := range []string{".", "@", "-", "_"} {
		if i := strings.Index(tag, sep); i > 0 {
			tag = tag[:i]
		}
	}
	return tag
}

// messages holds the per-language tables. Translations use indexed format
// verbs (%[2]s) where a language needs a different argument order than the
// English source string.
var messages = map[string]map[string]string{
	"en": {
		"alert.daily_total":  "Today's traffic passed %s (now %s).",
		"alert.app_total":    "%s used %s today (limit %s).",
		"alert.daily_upload": "⬆ Uploads passed %s today (now %s).",
		"alert.app_upload":   "⬆ %s uploaded %s today (limit %s).",
		"alert.plan_warn":    "Tethering data at %.0f%% of your %s plan (%s used, %s left).",
		"alert.plan_over":    "Tethering data plan used up — %s of %s this cycle.",

		"menubar.style.arrows":    "Arrows",
		"menubar.style.triangles": "Triangles",
		"menubar.style.caret":     "Carets",
		"menubar.style.suffix":    "Suffix",
		"menubar.style.downonly":  "Download only",
		"menubar.style.icononly":  "Icon only",
	},
	"ko": {
		"alert.daily_total":  "오늘 트래픽이 %s를 넘었습니다 (현재 %s).",
		"alert.app_total":    "%s이(가) 오늘 %s를 사용했습니다 (한도 %s).",
		"alert.daily_upload": "⬆ 오늘 업로드가 %s를 넘었습니다 (현재 %s).",
		"alert.app_upload":   "⬆ %s이(가) 오늘 %s를 업로드했습니다 (한도 %s).",
		"alert.plan_warn":    "테더링 데이터가 %[2]s 요금제의 %.0[1]f%%에 도달했습니다 (%[3]s 사용, %[4]s 남음).",
		"alert.plan_over":    "테더링 요금제를 다 썼습니다 — 이번 주기에 %[2]s 중 %[1]s 사용.",

		"menubar.style.arrows":    "화살표",
		"menubar.style.triangles": "삼각형",
		"menubar.style.caret":     "캐럿",
		"menubar.style.suffix":    "숫자 뒤 기호",
		"menubar.style.downonly":  "다운로드만",
		"menubar.style.icononly":  "아이콘만",
	},
	"ja": {
		"alert.daily_total":  "本日の通信量が %s を超えました（現在 %s）。",
		"alert.app_total":    "%s が本日 %s 使用しました（上限 %s）。",
		"alert.daily_upload": "⬆ 本日のアップロードが %s を超えました（現在 %s）。",
		"alert.app_upload":   "⬆ %s が本日 %s アップロードしました（上限 %s）。",
		"alert.plan_warn":    "テザリング通信量が %[2]s プランの %.0[1]f%% に達しました（%[3]s 使用・残り %[4]s）。",
		"alert.plan_over":    "テザリングプランを使い切りました — 今サイクル %[2]s 中 %[1]s。",

		"menubar.style.arrows":    "矢印",
		"menubar.style.triangles": "三角形",
		"menubar.style.caret":     "キャレット",
		"menubar.style.suffix":    "数値の後に記号",
		"menubar.style.downonly":  "ダウンロードのみ",
		"menubar.style.icononly":  "アイコンのみ",
	},
}
