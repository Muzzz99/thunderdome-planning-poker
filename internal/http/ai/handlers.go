package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Service 用于处理AI相关服务
type Service struct {
	AiApiKey string
	AiApiUrl string
	AiModel  string
}

// NewAIService 创建一个新的AI服务
func NewAIService() *Service {
	return &Service{
		AiApiKey: os.Getenv("THUNDERDOME_AI_API_KEY"),
		AiApiUrl: os.Getenv("THUNDERDOME_AI_API_URL"),
		AiModel:  os.Getenv("THUNDERDOME_AI_MODEL"),
	}
}

// 故事点数建议请求结构
type PointSuggestionRequest struct {
	StoryName          string   `json:"storyName"`
	Description        string   `json:"description"`
	AcceptanceCriteria string   `json:"acceptanceCriteria"`
	AvailablePoints    []string `json:"availablePoints"`
}

// 故事点数建议响应结构
type PointSuggestionResponse struct {
	SuggestedPoint string `json:"suggestedPoint"`
	Reason         string `json:"reason"`
}

// Hugging Face API请求结构
type HuggingFaceRequest struct {
	Inputs     string                 `json:"inputs"`
	Parameters map[string]interface{} `json:"parameters,omitempty"`
}

// Hugging Face API响应结构 - 根据模型不同可能返回不同格式
// 这里处理通用的文本响应格式
type HuggingFaceResponse []struct {
	GeneratedText string `json:"generated_text"`
}

// SuggestPoints 处理故事点数建议的请求
func (s *Service) SuggestPoints(w http.ResponseWriter, r *http.Request) {
	// 只允许POST请求
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed)
		return
	}

	// 从请求体中读取数据
	var req PointSuggestionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// 检查API密钥和URL是否已配置
	if s.AiApiUrl == "" {
		http.Error(w, "AI API not configured", http.StatusInternalServerError)
		return
	}

	// 构建发送给AI的提示
	prompt := buildAIPrompt(req)

	// 创建Hugging Face API请求
	aiReq := HuggingFaceRequest{
		Inputs: prompt,
		Parameters: map[string]interface{}{
			"max_new_tokens":   200,
			"temperature":      0.7,
			"top_p":            0.95,
			"return_full_text": false,
		},
	}

	// 将请求序列化为JSON
	aiReqBody, err := json.Marshal(aiReq)
	if err != nil {
		http.Error(w, "Error creating AI request", http.StatusInternalServerError)
		return
	}

	// 创建HTTP客户端并设置超时
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// 创建HTTP请求
	aiRequest, err := http.NewRequest("POST", s.AiApiUrl, bytes.NewBuffer(aiReqBody))
	if err != nil {
		http.Error(w, "Error creating HTTP request", http.StatusInternalServerError)
		return
	}

	// 设置请求头
	aiRequest.Header.Set("Content-Type", "application/json")
	if s.AiApiKey != "" {
		aiRequest.Header.Set("Authorization", "Bearer "+s.AiApiKey)
	}

	// 发送请求
	aiResp, err := client.Do(aiRequest)
	if err != nil {
		http.Error(w, "Error calling AI API: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer aiResp.Body.Close()

	// 读取响应体
	aiRespBody, err := ioutil.ReadAll(aiResp.Body)
	if err != nil {
		http.Error(w, "Error reading AI API response", http.StatusInternalServerError)
		return
	}

	// 检查响应状态码
	if aiResp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("AI API returned an error: %d - %s", aiResp.StatusCode, string(aiRespBody)), http.StatusInternalServerError)
		return
	}

	// 解析Hugging Face响应
	var hfResponse HuggingFaceResponse
	if err := json.Unmarshal(aiRespBody, &hfResponse); err != nil {
		// 尝试解析为纯文本响应
		suggestedPoint, reason := parseAIResponse(string(aiRespBody), req.AvailablePoints)

		// 准备响应
		response := PointSuggestionResponse{
			SuggestedPoint: suggestedPoint,
			Reason:         reason,
		}

		// 将响应发送回客户端
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
		return
	}

	// 如果成功解析为HuggingFaceResponse
	if len(hfResponse) > 0 && hfResponse[0].GeneratedText != "" {
		suggestedPoint, reason := parseAIResponse(hfResponse[0].GeneratedText, req.AvailablePoints)

		// 准备响应
		response := PointSuggestionResponse{
			SuggestedPoint: suggestedPoint,
			Reason:         reason,
		}

		// 将响应发送回客户端
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
		return
	}

	// 如果无法解析响应
	http.Error(w, "Unable to parse AI response", http.StatusInternalServerError)
}

// 构建发送给AI的提示文本
func buildAIPrompt(req PointSuggestionRequest) string {
	var prompt strings.Builder

	prompt.WriteString("作为敏捷估算专家，请为以下用户故事提供一个点数估计，并给出理由。\n\n")
	prompt.WriteString("故事名称: " + req.StoryName + "\n")

	if req.Description != "" {
		prompt.WriteString("描述: " + req.Description + "\n")
	}

	if req.AcceptanceCriteria != "" {
		prompt.WriteString("验收标准: " + req.AcceptanceCriteria + "\n")
	}

	prompt.WriteString("\n可用的点数值: " + joinStrings(req.AvailablePoints) + "\n\n")
	prompt.WriteString("请以JSON格式回复，结构为：{\"suggestedPoint\": \"<点数>\", \"reason\": \"<理由>\"}")

	return prompt.String()
}

// 解析AI响应并提取建议的点数和理由，限制点数在可用值范围内
func parseAIResponse(content string, availablePoints []string) (string, string) {
	// 尝试从回复中提取JSON
	content = strings.TrimSpace(content)

	// 将availablePoints转换为集合，便于查找
	validPoints := make(map[string]bool)
	for _, p := range availablePoints {
		validPoints[p] = true
	}

	// 查找JSON格式的回复
	jsonStart := strings.Index(content, "{")
	jsonEnd := strings.LastIndex(content, "}")

	if jsonStart >= 0 && jsonEnd > jsonStart {
		jsonContent := content[jsonStart : jsonEnd+1]
		var response PointSuggestionResponse
		err := json.Unmarshal([]byte(jsonContent), &response)

		if err == nil && response.SuggestedPoint != "" {
			// 验证点数是否在可用值范围内
			if validPoints[response.SuggestedPoint] {
				return response.SuggestedPoint, response.Reason
			} else {
				// 如果不在范围内，寻找最接近的值
				closestPoint := findClosestPoint(response.SuggestedPoint, availablePoints)
				return closestPoint, response.Reason
			}
		}
	}

	// 如果无法解析为JSON，尝试提取关键信息
	// 查找点数

	// 首先检查常见的点数模式
	for _, point := range availablePoints {
		// 常见的模式包括 "点数: 5", "suggestedPoint":"5", "建议点数: 5" 等
		patterns := []string{
			"点数: " + point,
			"suggestedPoint\":\"" + point,
			"建议点数: " + point,
			"我建议用 " + point,
			"推荐 " + point,
			"使用 " + point,
			"分配 " + point,
			point + " 点",
		}

		for _, pattern := range patterns {
			if strings.Contains(content, pattern) {
				return point, extractReason(content)
			}
		}
	}

	// 如果没有找到精确匹配，尝试查找任何数字
	foundNumber := findNumberInContent(content)
	if foundNumber != "" {
		closestPoint := findClosestPoint(foundNumber, availablePoints)
		return closestPoint, extractReason(content)
	}

	// 默认返回问号
	return "?", extractReason(content)
}

// 从内容中提取理由
func extractReason(content string) string {
	// 先尝试解析JSON格式的理由
	jsonStart := strings.Index(content, "{")
	jsonEnd := strings.LastIndex(content, "}")

	if jsonStart >= 0 && jsonEnd > jsonStart {
		jsonContent := content[jsonStart : jsonEnd+1]
		var response PointSuggestionResponse
		err := json.Unmarshal([]byte(jsonContent), &response)

		if err == nil && response.Reason != "" {
			return response.Reason
		}
	}

	// 如果不是JSON格式，尝试通过关键词提取
	reasonPrefixes := []string{"理由:", "原因:", "reason:", "理由是", "我的理由是"}
	for _, prefix := range reasonPrefixes {
		if idx := strings.Index(strings.ToLower(content), strings.ToLower(prefix)); idx >= 0 {
			reason := content[idx+len(prefix):]
			// 限制理由长度
			if len(reason) > 300 {
				reason = reason[:300] + "..."
			}
			return strings.TrimSpace(reason)
		}
	}

	// 如果没有找到明确的理由标记，使用整个内容作为理由
	// 但删除任何JSON格式或点数信息
	reason := content

	// 删除常见的点数声明模式
	patterns := []string{
		"点数: [0-9?]+",
		"建议点数: [0-9?]+",
		"我建议用 [0-9?]+",
		"suggestedPoint\":\"[0-9?]+\"",
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		reason = re.ReplaceAllString(reason, "")
	}

	// 删除JSON格式
	if jsonStart >= 0 && jsonEnd > jsonStart {
		if jsonStart > 0 {
			prefix := reason[:jsonStart]
			suffix := ""
			if jsonEnd+1 < len(reason) {
				suffix = reason[jsonEnd+1:]
			}
			reason = prefix + suffix
		} else {
			reason = reason[jsonEnd+1:]
		}
	}

	// 清理和限制理由长度
	reason = strings.TrimSpace(reason)
	if len(reason) > 300 {
		reason = reason[:300] + "..."
	}

	return reason
}

// 从内容中查找任何数字
func findNumberInContent(content string) string {
	// 使用正则表达式查找数字
	re := regexp.MustCompile("[0-9]+")
	matches := re.FindAllString(content, -1)

	if len(matches) > 0 {
		return matches[0]
	}

	return ""
}

// 寻找最接近的点数
func findClosestPoint(point string, availablePoints []string) string {
	// 如果点数已经在可用点数中，直接返回
	for _, p := range availablePoints {
		if p == point {
			return p
		}
	}

	// 如果是问号或咖啡杯，直接返回问号
	if point == "?" || point == "☕️" {
		for _, p := range availablePoints {
			if p == "?" {
				return p
			}
		}
		// 如果没有问号，返回最后一个点数（通常是问号或最大值）
		return availablePoints[len(availablePoints)-1]
	}

	// 如果是半个点 "1/2"，直接返回1
	if point == "1/2" {
		for _, p := range availablePoints {
			if p == "1/2" {
				return p
			}
		}
		for _, p := range availablePoints {
			if p == "1" {
				return p
			}
		}
	}

	// 尝试转换为数字进行比较
	num, err := strconv.Atoi(point)
	if err != nil {
		// 如果无法转换为数字，返回问号
		for _, p := range availablePoints {
			if p == "?" {
				return p
			}
		}
		return availablePoints[len(availablePoints)-1]
	}

	// 通过数字比较找到最接近的点数
	closest := "?"
	minDiff := math.MaxInt32

	for _, p := range availablePoints {
		// 跳过非数字点数
		if p == "?" || p == "☕️" || p == "1/2" {
			continue
		}

		pNum, err := strconv.Atoi(p)
		if err != nil {
			continue
		}

		diff := int(math.Abs(float64(pNum - num)))
		if diff < minDiff {
			minDiff = diff
			closest = p
		}
	}

	return closest
}

// 辅助函数，将字符串数组连接为逗号分隔的字符串
func joinStrings(strings []string) string {
	var result string
	for i, s := range strings {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
