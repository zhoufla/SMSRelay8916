// Package otp 从短信正文中提取验证码。
//
// 只做克制的匹配：优先「关键词上下文」，实在没有结构线索、且正文带验证类字样时
// 才兜底取独立数字 —— 避免把金额、年份、订单号、手机号片段误当验证码。
// 提取结果只是展示辅助（高亮 / 复制 / 转发模板），正文永远完整保留。
package otp

import (
	"regexp"
)

// 关键词在前：验证码：483921 / 您的验证码是 582913 / Your code is 483921。
//
// 尾部 \b 防止吞进更长的数字串：RE2 没有 lookbehind，但词边界足够 ——
// 11 位手机号、10 位订单号内部不存在词边界，\d{4,8}\b 在其中匹配不上。
// 分隔符排除数字与换行，所以不会「跳过」一个短数字去抓后面的，也不会跨行。
var keywordFirstRe = regexp.MustCompile(
	`(?:验证码|校验码|确认码|动态码|动态密码|动态口令|(?i:verification code|code is|code|otp))` +
		`[^0-9\n]{0,8}(\d{4,8})\b`)

// 数字在前：483921 是您的验证码 / 483921（验证码）/ 123456 is your code。
// 头部 \b 防止从长数字串中间截出一段（无头边界时 \d{4,8} 可能从第 3 位开始匹配）。
var digitFirstRe = regexp.MustCompile(
	`\b(\d{4,8})\b(?:[^0-9\n]{0,6}(?:验证码|校验码)` +
		`|[^0-9\n]{0,10}(?i:is your (?:verification |security |login )?code))`)

// hintRe 判断正文是否带验证类字样 —— 兜底提取的前置条件。
var hintRe = regexp.MustCompile(`验证|校验|(?i:otp|code)`)

// bareRe 是兜底用的独立 4-8 位数字。
var bareRe = regexp.MustCompile(`\b\d{4,8}\b`)

// Extract 返回正文中疑似验证码的数字串；没有把握时返回空串。
func Extract(body string) string {
	for _, re := range []*regexp.Regexp{keywordFirstRe, digitFirstRe} {
		if m := re.FindStringSubmatch(body); m != nil {
			return m[1]
		}
	}
	if !hintRe.MatchString(body) {
		return ""
	}
	// 兜底：跳过被 * / # 打码的号码尾巴（如 138****1234 的 1234）
	for _, loc := range bareRe.FindAllStringIndex(body, -1) {
		if loc[0] > 0 && isMaskByte(body[loc[0]-1]) {
			continue
		}
		if loc[1] < len(body) && isMaskByte(body[loc[1]]) {
			continue
		}
		return body[loc[0]:loc[1]]
	}
	return ""
}

// isMaskByte 判断数字两侧是否是打码符号。'*'、'#' 是 ASCII，
// UTF-8 多字节序列的续字节不会与 ASCII 值相同，按字节比较安全。
func isMaskByte(b byte) bool { return b == '*' || b == '#' }
