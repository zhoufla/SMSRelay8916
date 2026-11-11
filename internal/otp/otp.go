// Package otp 从短信原文中提取验证码；无法确定唯一结果时返回空串。
package otp

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const englishKeywords = `\b(?i:verification code|authentication code|security code|confirmation code|login code|authorization code|one-time code|one time code|one-time password|one time password|passcode|otp)\b`

var (
	// 扫描完整串再判断长度，不从手机号、长编号或带下划线的串中截取。
	tokenRe  = regexp.MustCompile(`[A-Za-z0-9_]+`)
	strongRe = regexp.MustCompile(
		`验证码|校验码|确认码|动态码|动态密码|动态口令|一次性密码|一次性口令|登录码|登陆码|注册码|安全码|授权码|` + englishKeywords)
	hintRe       = regexp.MustCompile(`验证|校验|` + englishKeywords)
	codeRe       = regexp.MustCompile(`\b(?i:code)\b`)
	codeBeforeRe = regexp.MustCompile(`(?i)^[\s\p{Z}]*(?:[:：]|is\b)[\s\p{Z}]*$`)
	codeAfterRe  = regexp.MustCompile(`(?i)^[\s\p{Z}]+is[\s\p{Z}]+your[\s\p{Z}]+$`)
	urlRe        = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s\p{Z}"'<>]+`)
	emailRe      = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+`)
	negativeRe   = regexp.MustCompile(
		`(?i)(?:手机号|电话|尾号|订单号|流水号|运单号|取件码|提货码|券码|优惠码|错误码|` +
			`\b(?:phone|mobile|order number|tracking number|pickup code|coupon code|error code)\b)` +
			`[\s\p{Z}:：]*(?:(?:是|为|\bis\b)[\s\p{Z}:：]*)?$`)
	unitRe = regexp.MustCompile(`^[\s\p{Z}]*(?:分钟|小时|年|月|日|时|点|秒|分|天|元|圆|角|%|‰|％)`)
	// 分组数字、小数、日期和时间中的片段都不能单独作为验证码。
	numberBeforeRe = regexp.MustCompile(`\p{Nd}(?:[\s\p{Z}]+|[\s\p{Z}]*[-.:：][\s\p{Z}]*)$`)
	numberAfterRe  = regexp.MustCompile(`^(?:[\s\p{Z}]+|[\s\p{Z}]*[-.:：][\s\p{Z}]*)\p{Nd}`)
)

// span 使用原文的字节位置，返回时直接切原文，保留大小写和前导零。
type span struct{ start, end int }

// Extract 只接受 4～8 位数字或字母数字混合串，按上下文层级选取唯一值。
func Extract(body string) string {
	blocked := append(urlRe.FindAllStringIndex(body, -1), emailRe.FindAllStringIndex(body, -1)...)
	strong := locations(strongRe, body, blocked)
	weak := locations(codeRe, body, blocked)
	hints := locations(hintRe, body, blocked)
	best, result, ambiguous := 3, "", false
	for _, loc := range tokenRe.FindAllStringIndex(body, -1) {
		candidate := span{loc[0], loc[1]}
		value := body[candidate.start:candidate.end]
		digits, valid := codeFormat(value)
		if !valid || overlaps(candidate, blocked) || excluded(body, candidate, digits) {
			continue
		}
		level := contextLevel(body, candidate, digits, strong, weak, hints)
		if level == 3 {
			continue
		}
		// 只比较最高有效层级；较低层级的歧义不影响更明确的结果。
		if level < best {
			best, result, ambiguous = level, value, false
		} else if level == best && value != result {
			ambiguous = true
		}
	}
	if ambiguous {
		return ""
	}
	return result
}

func codeFormat(value string) (digits, valid bool) {
	if len(value) < 4 || len(value) > 8 {
		return false, false
	}
	hasDigit, hasLetter := false, false
	for _, c := range value {
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			hasLetter = true
		default:
			return false, false
		}
	}
	return !hasLetter, hasDigit
}

func locations(re *regexp.Regexp, body string, blocked [][]int) []span {
	var out []span
	for _, loc := range re.FindAllStringIndex(body, -1) {
		s := span{loc[0], loc[1]}
		if !overlaps(s, blocked) {
			out = append(out, s)
		}
	}
	return out
}

func overlaps(s span, blocked [][]int) bool {
	for _, loc := range blocked {
		if s.start < loc[1] && loc[0] < s.end {
			return true
		}
	}
	return false
}

func excluded(body string, candidate span, digits bool) bool {
	before, after := body[:candidate.start], body[candidate.end:]
	left, _ := utf8.DecodeLastRuneInString(before)
	right, _ := utf8.DecodeRuneInString(after)
	if strings.ContainsRune("*#", left) || strings.ContainsRune("*#", right) ||
		isTokenNeighbor(left) || isTokenNeighbor(right) {
		return true
	}
	if negativeRe.MatchString(before) || numberBeforeRe.MatchString(before) || numberAfterRe.MatchString(after) {
		return true
	}
	if digits {
		currency, _ := utf8.DecodeLastRuneInString(strings.TrimRightFunc(before, unicode.IsSpace))
		return strings.ContainsRune("¥￥$€£", currency) || unitRe.MatchString(after)
	}
	return false
}

// 全角字母、Unicode 数字不能成为截取半角片段的边界；中文仍可紧贴验证码。
func isTokenNeighbor(r rune) bool {
	return unicode.IsDigit(r) || r >= 'Ａ' && r <= 'Ｚ' || r >= 'ａ' && r <= 'ｚ'
}

// gapBetween 不允许跨过句末标点或空行；距离按 Unicode 字符计算。
func gapBetween(body string, a, b span, limit int) (string, bool) {
	if a.start > b.start {
		a, b = b, a
	}
	if a.end > b.start {
		return "", false
	}
	gap := body[a.end:b.start]
	return gap, utf8.RuneCountInString(gap) <= limit &&
		strings.Count(gap, "\n") <= 1 && !strings.ContainsAny(gap, "。！？!?；;")
}

func contextLevel(body string, candidate span, digits bool, strong, weak, hints []span) int {
	for _, keyword := range strong {
		if gap, ok := gapBetween(body, candidate, keyword, 24); ok && strings.IndexFunc(gap, unicode.IsDigit) < 0 {
			return 0
		}
	}
	for _, keyword := range weak {
		gap, ok := gapBetween(body, candidate, keyword, 24)
		if ok && (keyword.end <= candidate.start && codeBeforeRe.MatchString(gap) ||
			candidate.end <= keyword.start && codeAfterRe.MatchString(gap)) {
			return 1
		}
	}
	if digits {
		for _, hint := range hints {
			if _, ok := gapBetween(body, candidate, hint, 64); ok {
				return 2
			}
		}
	}
	return 3
}
