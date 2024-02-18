package manager

import (
	"chat/adapter"
	"chat/addition/web"
	"chat/admin"
	"chat/auth"
	"chat/channel"
	"chat/globals"
	"chat/utils"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	ReasonStop      = "stop"
	ReasonToolsCall = "tools_call"
)

func supportRelayPlan() bool {
	return channel.SystemInstance.SupportRelayPlan()
}

func ChatRelayAPI(c *gin.Context) {
	username := utils.GetUserFromContext(c)
	if username == "" {
		abortWithErrorResponse(c, fmt.Errorf("access denied for invalid api key"), "authentication_error")
		return
	}

	if utils.GetAgentFromContext(c) != "api" {
		abortWithErrorResponse(c, fmt.Errorf("access denied for invalid agent"), "authentication_error")
		return
	}

	var form RelayForm
	if err := c.ShouldBindJSON(&form); err != nil {
		abortWithErrorResponse(c, fmt.Errorf("invalid request body: %s", err.Error()), "invalid_request_error")
		return
	}

	db := utils.GetDBFromContext(c)
	user := &auth.User{
		Username: username,
	}
	id := utils.Md5Encrypt(username + form.Model + time.Now().String())
	created := time.Now().Unix()

	messages := transform(form.Messages)
	if strings.HasPrefix(form.Model, "web-") {
		suffix := strings.TrimPrefix(form.Model, "web-")

		form.Model = suffix
		messages = web.UsingWebNativeSegment(true, messages)
	}

	if strings.HasSuffix(form.Model, "-official") {
		form.Model = strings.TrimSuffix(form.Model, "-official")
		form.Official = true
	}

	check := auth.CanEnableModel(db, user, form.Model)
	if check != nil {
		sendErrorResponse(c, check, "quota_exceeded_error")
		return
	}

	if form.Stream {
		sendStreamTranshipmentResponse(c, form, messages, id, created, user, supportRelayPlan())
	} else {
		sendTranshipmentResponse(c, form, messages, id, created, user, supportRelayPlan())
	}
}

func getChatProps(form RelayForm, messages []globals.Message, buffer *utils.Buffer, plan bool) *adapter.ChatProps {
	return &adapter.ChatProps{
		Model:             form.Model,
		Message:           messages,
		MaxTokens:         form.MaxTokens,
		PresencePenalty:   form.PresencePenalty,
		FrequencyPenalty:  form.FrequencyPenalty,
		RepetitionPenalty: form.RepetitionPenalty,
		Temperature:       form.Temperature,
		TopP:              form.TopP,
		TopK:              form.TopK,
		Tools:             form.Tools,
		ToolChoice:        form.ToolChoice,
		Buffer:            *buffer,
	}
}

func sendTranshipmentResponse(c *gin.Context, form RelayForm, messages []globals.Message, id string, created int64, user *auth.User, plan bool) {
	db := utils.GetDBFromContext(c)
	cache := utils.GetCacheFromContext(c)

	buffer := utils.NewBuffer(form.Model, messages, channel.ChargeInstance.GetCharge(form.Model))
	hit, err := channel.NewChatRequestWithCache(cache, buffer, auth.GetGroup(db, user), getChatProps(form, messages, buffer, plan), func(data *globals.Chunk) error {
		buffer.WriteChunk(data)
		return nil
	})

	admin.AnalysisRequest(form.Model, buffer, err)
	if err != nil {
		auth.RevertSubscriptionUsage(db, cache, user, form.Model)
		globals.Warn(fmt.Sprintf("error from chat request api: %s (instance: %s, client: %s)", err, form.Model, c.ClientIP()))

		sendErrorResponse(c, err)
		return
	}

	if !hit {
		CollectQuota(c, user, buffer, plan, err)
	}

	tools := buffer.GetToolCalls()

	c.JSON(http.StatusOK, RelayResponse{
		Id:      fmt.Sprintf("chatcmpl-%s", id),
		Object:  "chat.completion",
		Created: created,
		Model:   form.Model,
		Choices: []Choice{
			{
				Index: 0,
				Message: globals.Message{
					Role:         globals.Assistant,
					Content:      buffer.Read(),
					ToolCalls:    tools,
					FunctionCall: buffer.GetFunctionCall(),
				},
				FinishReason: utils.Multi(tools != nil, ReasonToolsCall, ReasonStop),
			},
		},
		Usage: Usage{
			PromptTokens:     buffer.CountInputToken(),
			CompletionTokens: buffer.CountOutputToken(),
			TotalTokens:      buffer.CountToken(),
		},
		Quota: utils.Multi[*float32](form.Official, nil, utils.ToPtr(buffer.GetQuota())),
	})
}

func getFinishReason(buffer *utils.Buffer, end bool) interface{} {
	if !end {
		return nil
	}

	if buffer.IsFunctionCalling() {
		return ReasonToolsCall
	}

	return ReasonStop
}

func getRole(data *globals.Chunk) string {
	if data.Content != "" {
		return globals.Assistant
	} else if data.ToolCall != nil {
		return globals.Tool
	} else if data.FunctionCall != nil {
		return globals.Function
	}

	return ""
}

func getStreamTranshipmentForm(id string, created int64, form RelayForm, data *globals.Chunk, buffer *utils.Buffer, end bool, err error) RelayStreamResponse {
	return RelayStreamResponse{
		Id:      fmt.Sprintf("chatcmpl-%s", id),
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   form.Model,
		Choices: []ChoiceDelta{
			{
				Index: 0,
				Delta: Message{
					Content:      data.Content,
					ToolCalls:    data.ToolCall,
					FunctionCall: data.FunctionCall,
				},
				FinishReason: getFinishReason(buffer, end),
			},
		},
		//Usage: Usage{
		//	PromptTokens:     utils.MultiF(end, func() int { return buffer.CountInputToken() }, 0),
		//	CompletionTokens: utils.MultiF(end, func() int { return buffer.CountOutputToken() }, 0),
		//	TotalTokens:      utils.MultiF(end, func() int { return buffer.CountToken() }, 0),
		//},
		//Quota: utils.Multi[*float32](form.Official, nil, utils.ToPtr(buffer.GetQuota())),
		Error: err,
	}
}

func sendStreamTranshipmentResponse(c *gin.Context, form RelayForm, messages []globals.Message, id string, created int64, user *auth.User, plan bool) {
	partial := make(chan RelayStreamResponse)
	db := utils.GetDBFromContext(c)
	cache := utils.GetCacheFromContext(c)

	group := auth.GetGroup(db, user)
	charge := channel.ChargeInstance.GetCharge(form.Model)

	go func() {
		buffer := utils.NewBuffer(form.Model, messages, charge)
		hit, err := channel.NewChatRequestWithCache(
			cache, buffer, group, getChatProps(form, messages, buffer, plan),
			func(data *globals.Chunk) error {
				buffer.WriteChunk(data)

				if !data.IsEmpty() {
					partial <- getStreamTranshipmentForm(id, created, form, data, buffer, false, nil)
				}
				return nil
			},
		)

		admin.AnalysisRequest(form.Model, buffer, err)
		if err != nil {
			auth.RevertSubscriptionUsage(db, cache, user, form.Model)
			globals.Warn(fmt.Sprintf("error from chat request api: %s (instance: %s, client: %s)", err.Error(), form.Model, c.ClientIP()))
			partial <- getStreamTranshipmentForm(id, created, form, &globals.Chunk{Content: err.Error()}, buffer, true, err)
			close(partial)
			return
		}

		partial <- getStreamTranshipmentForm(id, created, form, &globals.Chunk{Content: ""}, buffer, true, nil)

		if !hit {
			CollectQuota(c, user, buffer, plan, err)
		}

		close(partial)
		return
	}()

	c.Stream(func(w io.Writer) bool {
		if resp, ok := <-partial; ok {
			if resp.Error != nil {
				sendErrorResponse(c, resp.Error)
				return false
			}

			c.Render(-1, utils.NewEvent(resp))
			return true
		}

		c.Render(-1, utils.NewEndEvent())
		return false
	})
}
