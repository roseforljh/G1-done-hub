package openai

import (
	"bytes"
	"done-hub/common"
	"done-hub/common/config"
	"done-hub/common/model_utils"
	"done-hub/common/requester"
	"done-hub/providers/base"
	"done-hub/types"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

type OpenAIStreamHandler struct {
	Usage      *types.Usage
	ModelName  string
	isAzure    bool
	EscapeJSON bool
	Context    *gin.Context // 添加 Context 用于获取响应模型名称

	ReasoningHandler bool
	ExtraBilling     map[string]types.ExtraBilling `json:"-"`
	UsageHandler     UsageHandler
}

func (p *OpenAIProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	if p.RequestHandleBefore != nil {
		errWithCode = p.RequestHandleBefore(request)
		if errWithCode != nil {
			return nil, errWithCode
		}
	}
	otherProcessing(request, p.GetOtherArg())

	// 对于自定义渠道，过滤空content的消息以保持与其他渠道一致的行为
	if p.Channel.Type == config.ChannelTypeCustom {
		request.Messages = common.FilterEmptyContentMessages(request.Messages)
	}

	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &OpenAIProviderChatResponse{}
	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, response, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// 检测是否错误
	openaiErr := ErrorHandle(&response.OpenAIErrorResponse)
	if openaiErr != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *openaiErr,
			StatusCode:  http.StatusBadRequest,
		}
		return nil, errWithCode
	}

	if response.Usage == nil || response.Usage.CompletionTokens == 0 {
		response.Usage = &types.Usage{
			PromptTokens:     p.Usage.PromptTokens,
			CompletionTokens: 0,
			TotalTokens:      0,
		}
		// 那么需要计算
		response.Usage.CompletionTokens = common.CountTokenText(response.GetContent(), request.Model)
		response.Usage.TotalTokens = response.Usage.PromptTokens + response.Usage.CompletionTokens
	} else if p.UsageHandler != nil {
		p.UsageHandler(response.Usage)
	}

	*p.Usage = *response.Usage

	p.Usage.ExtraBilling = getChatExtraBilling(request)

	// 修改响应中的模型名称为用户请求的原始模型名称
	responseModel := p.GetResponseModelName(request.Model)
	response.Model = responseModel

	return &response.ChatCompletionResponse, nil
}

func (p *OpenAIProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	if p.RequestHandleBefore != nil {
		errWithCode := p.RequestHandleBefore(request)
		if errWithCode != nil {
			return nil, errWithCode
		}
	}
	otherProcessing(request, p.GetOtherArg())

	// 对于自定义渠道，过滤空content的消息以保持与其他渠道一致的行为
	if p.Channel.Type == config.ChannelTypeCustom {
		request.Messages = common.FilterEmptyContentMessages(request.Messages)
	}

	streamOptions := request.StreamOptions
	// 如果支持流式返回Usage 则需要更改配置：
	if p.SupportStreamOptions {
		request.StreamOptions = &types.StreamOptions{
			IncludeUsage: true,
		}
	} else {
		// 避免误传导致报错
		request.StreamOptions = nil
	}
	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// 恢复原来的配置
	request.StreamOptions = streamOptions

	// 发送请求
	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := OpenAIStreamHandler{
		Usage:      p.Usage,
		ModelName:  request.Model,
		isAzure:    p.IsAzure,
		EscapeJSON: p.StreamEscapeJSON,
		Context:    p.Context, // 传递 Context

		ExtraBilling: getChatExtraBilling(request),
		UsageHandler: p.UsageHandler,
	}

	return requester.RequestStream(p.Requester, resp, chatHandler.HandlerChatStream)
}

func (h *OpenAIStreamHandler) HandlerChatStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(string(*rawLine), "data:") {
		*rawLine = nil
		return
	}

	// 去除前缀
	*rawLine = (*rawLine)[5:]
	*rawLine = bytes.TrimSpace(*rawLine)

	// 如果等于 DONE 则结束
	if string(*rawLine) == "[DONE]" {
		errChan <- io.EOF
		*rawLine = requester.StreamClosed
		return
	}

	var openaiResponse OpenAIProviderChatStreamResponse
	err := json.Unmarshal(*rawLine, &openaiResponse)
	if err != nil {
		errChan <- common.ErrorToOpenAIError(err)
		return
	}

	aiError := ErrorHandle(&openaiResponse.OpenAIErrorResponse)
	if aiError != nil {
		errChan <- aiError
		return
	}

	if openaiResponse.Usage != nil {
		if openaiResponse.Usage.CompletionTokens > 0 {
			if h.UsageHandler != nil && h.UsageHandler(openaiResponse.Usage) {
				h.EscapeJSON = true
			}
			*h.Usage = *openaiResponse.Usage

			if h.ExtraBilling != nil {
				h.Usage.ExtraBilling = h.ExtraBilling
			}
		}

		if len(openaiResponse.Choices) == 0 {
			*rawLine = nil
			return
		}
	} else {
		if len(openaiResponse.Choices) > 0 && openaiResponse.Choices[0].Usage != nil {
			if openaiResponse.Choices[0].Usage.CompletionTokens > 0 {
				if h.UsageHandler != nil && h.UsageHandler(openaiResponse.Choices[0].Usage) {
					h.EscapeJSON = true
				}
				*h.Usage = *openaiResponse.Choices[0].Usage
				if h.ExtraBilling != nil {
					h.Usage.ExtraBilling = h.ExtraBilling
				}
			}
		} else {
			if h.Usage.TotalTokens == 0 {
				h.Usage.TotalTokens = h.Usage.PromptTokens
			}
		}
	}

	// 修改响应中的模型名称为用户请求的原始模型名称
	if h.Context != nil {
		responseModel := base.GetResponseModelNameFromContext(h.Context, openaiResponse.Model)
		openaiResponse.Model = responseModel
	}

	// 始终累积流式内容到 TextBuilder，用于流中断时的 token 计算备用
	// 即使上游返回了 Usage 信息，流中断时最终的 Usage 可能不完整
	responseText := openaiResponse.GetResponseText()
	if responseText != "" {
		h.Usage.TextBuilder.WriteString(responseText)
	}

	if h.ReasoningHandler && len(openaiResponse.Choices) > 0 {
		for index, choices := range openaiResponse.Choices {
			if choices.Delta.ReasoningContent == "" && choices.Delta.Reasoning != "" {
				openaiResponse.Choices[index].Delta.ReasoningContent = choices.Delta.Reasoning
				openaiResponse.Choices[index].Delta.Reasoning = ""
			}
		}

		h.EscapeJSON = true
	}

	if h.EscapeJSON {
		if data, err := json.Marshal(openaiResponse.ChatCompletionStreamResponse); err == nil {
			dataChan <- string(data)
			return
		}
	}
	dataChan <- string(*rawLine)
}

func otherProcessing(request *types.ChatCompletionRequest, otherArg string) {
	matched, _ := regexp.MatchString(`(?i)^o[1-9]`, request.Model)
	if matched || model_utils.HasPrefixCaseInsensitive(request.Model, "gpt-5") {
		if request.MaxTokens > 0 {
			request.MaxCompletionTokens = request.MaxTokens
			request.MaxTokens = 0
		}
		if request.Model != "gpt-5-chat-latest" {
			request.Temperature = nil
		}
		if otherArg != "" {
			request.ReasoningEffort = &otherArg
		}
	}
}

func getChatExtraBilling(request *types.ChatCompletionRequest) map[string]types.ExtraBilling {
	if !strings.Contains(request.Model, "search-preview") {
		return nil
	}

	searchType := "medium"
	if request.WebSearchOptions != nil && request.WebSearchOptions.SearchContextSize != "" {
		searchType = request.WebSearchOptions.SearchContextSize
	}

	return map[string]types.ExtraBilling{
		types.APITollTypeWebSearchPreview: {
			Type:      searchType,
			CallCount: 1,
		},
	}
}
