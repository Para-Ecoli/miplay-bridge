// Hardware mixer endpoints: GET /api/mixer serves the full control state of
// the ALSA card and POST /api/mixer writes one control. The request and
// response shapes carry over from the former standalone webmixer server,
// with the bridge token as the only authentication layer.
package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/miplay-bridge/miplay-bridge/internal/mix"
)

// mixerRequest is the POST /api/mixer body: volume and switch are mutually
// exclusive, exactly one of them must be present.
type mixerRequest struct {
	Name   string `json:"name"`   // control name, e.g. "Headphone"
	Stream string `json:"stream"` // "playback" (default) or "capture"
	Volume []int  `json:"volume"` // raw hardware values in channel order
	Switch string `json:"switch"` // "on" / "off" / "toggle"
}

// mixerState returns the complete card state (scontents + info), the shape
// the console panel polls.
func (s *Server) mixerState(w http.ResponseWriter, r *http.Request) {
	if s.mixer == nil {
		writeError(w, http.StatusServiceUnavailable, "hardware mixer is unavailable", "mixer_unavailable")
		return
	}
	state, err := s.mixer.State(r.Context())
	if err != nil {
		s.writeMixerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "mixer": state})
}

// mixerSet validates one write request against the live card state, applies
// it and answers with the post-write full state so the caller realigns with
// one round trip.
func (s *Server) mixerSet(w http.ResponseWriter, r *http.Request) {
	var request mixerRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error(), "invalid_request")
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		writeError(w, http.StatusBadRequest, "缺少控件名 name", "invalid_request")
		return
	}
	if request.Stream == "" {
		request.Stream = "playback"
	}
	if request.Stream != "playback" && request.Stream != "capture" {
		writeError(w, http.StatusBadRequest, "stream 只能为 playback 或 capture", "invalid_request")
		return
	}
	hasVolume := len(request.Volume) > 0
	hasSwitch := request.Switch != ""
	if hasVolume == hasSwitch {
		writeError(w, http.StatusBadRequest, "volume 与 switch 必须二选一", "invalid_request")
		return
	}
	if hasSwitch && request.Switch != "on" && request.Switch != "off" && request.Switch != "toggle" {
		writeError(w, http.StatusBadRequest, "switch 只能为 on / off / toggle", "invalid_request")
		return
	}
	if s.mixer == nil {
		writeError(w, http.StatusServiceUnavailable, "hardware mixer is unavailable", "mixer_unavailable")
		return
	}

	ctx := r.Context()
	// Read the live state first: it validates the control and drives the
	// uniform-value expansion, mirroring the reference implementation.
	state, err := s.mixer.State(ctx)
	if err != nil {
		s.writeMixerError(w, err)
		return
	}
	control := mix.FindControl(state, request.Name)
	if control == nil {
		writeError(w, http.StatusBadRequest, "未找到控件 "+request.Name, "invalid_request")
		return
	}
	if control.Index != 0 {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("控件 %s 的 index 为 %d，无法按名操作", request.Name, control.Index), "invalid_request")
		return
	}

	if hasVolume {
		limit := control.LimitFor(request.Stream)
		if limit == nil {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("控件 %s 不支持 %s 音量", request.Name, request.Stream), "invalid_request")
			return
		}
		channels := control.VolumeChannelCount(request.Stream)
		if channels == 0 {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("控件 %s 没有 %s 声道", request.Name, request.Stream), "invalid_request")
			return
		}
		// 统一控制：只给一个值时应用到所有声道
		if len(request.Volume) == 1 {
			single := request.Volume[0]
			request.Volume = make([]int, channels)
			for i := range request.Volume {
				request.Volume[i] = single
			}
		}
		if len(request.Volume) != channels {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("控件 %s 需要 1 个或 %d 个音量值", request.Name, channels), "invalid_request")
			return
		}
		for i, value := range request.Volume {
			if value < limit.Min {
				value = limit.Min
			} else if value > limit.Max {
				value = limit.Max
			}
			request.Volume[i] = value
		}
	}

	if hasVolume {
		err = s.mixer.SetVolume(ctx, request.Name, request.Stream, request.Volume)
	} else {
		err = s.mixer.SetSwitch(ctx, request.Name, request.Stream, request.Switch)
	}
	if err != nil {
		s.writeMixerError(w, err)
		return
	}

	updated, err := s.mixer.State(ctx)
	if err != nil {
		s.writeMixerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "mixer": updated})
}

// writeMixerError maps amixer failures onto structured HTTP errors: a
// missing amixer binary degrades to 503 mixer_unavailable, any other control
// error to 502 mixer_error.
func (s *Server) writeMixerError(w http.ResponseWriter, err error) {
	if mix.IsUnavailable(err) {
		writeError(w, http.StatusServiceUnavailable, "hardware mixer is unavailable: "+err.Error(), "mixer_unavailable")
		return
	}
	writeError(w, http.StatusBadGateway, "hardware mixer error: "+err.Error(), "mixer_error")
}
