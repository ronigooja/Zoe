package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"
)

const DefaultInstructionText = "你是语音入口。问候、感谢、闲聊、观点这类不需要查资料的话,你自己直接回答。其余一律委托,不要自己回答:需要查资料、联网、操作文件或设备的请求;问之前交办的事做到哪了、好了没有;问以前的通话里说过什么、做过什么;让你记住或回忆我的个人信息。委托之后等结果再说,不要自己编结果,也不要说没有记录。"
const InstructionPrefix = "Standing rule for you, do not read this aloud or acknowledge it: "

// ResolveInstructionText validates startup configuration; empty uses the default.
func ResolveInstructionText(text string) (string, error) {
	if !utf8.ValidString(text) || len(text) > 6000 {
		return "", errors.New("ZOE_VOICE_INSTRUCTION_TEXT must be valid UTF-8 and at most 6000 bytes")
	}
	if text == "off" {
		return "", nil
	}
	if text == "" {
		return DefaultInstructionText, nil
	}
	return text, nil
}

// deliverInstruction applies on creation and once more on connection after failure.
// Durable claims prevent duplicate sends; ambiguous acknowledgements are not exactly-once.
func (s *Service) deliverInstruction(ctx context.Context, session string, retry bool) {
	text, err := ResolveInstructionText(s.recall.InstructionText)
	if err != nil || text == "" {
		return
	}
	var gateway string
	var generation int64
	if err := s.db.QueryRowContext(ctx, `SELECT gateway_session_id,gateway_media_generation FROM voice_sessions WHERE id=? AND state='active'`, session).Scan(&gateway, &generation); err != nil || gateway == "" {
		return
	}
	hash := sha256.Sum256([]byte(text))
	version := hex.EncodeToString(hash[:8])
	result, err := s.db.ExecContext(ctx, `INSERT INTO voice_instruction_deliveries(session_id,media_generation,instruction_version,instruction_delivery,attempts) VALUES(?,?,?,'sending',1) ON CONFLICT DO NOTHING`, session, generation, version)
	if err != nil {
		log.Printf("voice instruction session_id=%s status=storage_failed", session)
		return
	}
	claimed, _ := result.RowsAffected()
	if claimed == 0 && retry {
		result, err = s.db.ExecContext(ctx, `UPDATE voice_instruction_deliveries SET instruction_delivery='sending',attempts=2 WHERE session_id=? AND media_generation=? AND instruction_version=? AND instruction_delivery='failed' AND attempts=1`, session, generation, version)
		if err == nil {
			claimed, _ = result.RowsAffected()
		}
	}
	if claimed != 1 || err != nil {
		return
	}
	// Never hold a database transaction during transport; bound call-start latency.
	pushCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"text": InstructionPrefix + text})
	response, _, callErr := s.callGateway(pushCtx, http.MethodPost, "/v1/voice/sessions/"+url.PathEscape(gateway)+"/context", body)
	delivery, status := "failed", 0
	if response != nil {
		status = response.StatusCode
		response.Body.Close()
		if callErr == nil && status >= 200 && status < 300 {
			delivery = "delivered"
		}
	}
	// Detached, bounded recording preserves failure evidence after request cancellation.
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer recordCancel()
	_, recordErr := s.db.ExecContext(recordCtx, `UPDATE voice_instruction_deliveries SET instruction_delivery=? WHERE session_id=? AND media_generation=? AND instruction_version=?`, delivery, session, generation, version)
	if recordErr != nil {
		log.Printf("voice instruction session_id=%s status=storage_failed", session)
	}
	log.Printf("voice instruction session_id=%s status=%d", session, status)
}
