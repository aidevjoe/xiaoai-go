package audio

import (
	"encoding/binary"
	"math"
)

// TonePCM 生成一段提示音的 PCM（S16LE、单声道）。
//
// gateMS > 0 时按"响 gateMS、停 gateMS"交替，用来做"叮—咚"这类双音提示；
// gateMS == 0 时整段连续。
// 首尾各做 5ms 淡入淡出，避免爆音（小爱扬声器功放对突变很敏感）。
func TonePCM(rate, freqHz, durMS, gateMS, amp int) []byte {
	if rate <= 0 {
		rate = 24000
	}
	if freqHz <= 0 {
		freqHz = 880
	}
	if durMS <= 0 {
		durMS = 120
	}
	if amp <= 0 || amp > 32767 {
		amp = 8000
	}

	total := rate * durMS / 1000
	out := make([]byte, total*2)
	rampLen := rate * 5 / 1000 // 5ms
	if rampLen*2 > total {
		rampLen = total / 4
	}

	on := true
	gateSamples := 0
	if gateMS > 0 {
		gateSamples = rate * gateMS / 1000
		if gateSamples < 1 {
			gateSamples = 1
		}
	}

	for i := 0; i < total; i++ {
		if gateSamples > 0 && i > 0 && i%gateSamples == 0 {
			on = !on
		}
		if !on {
			continue // 留静音
		}
		v := math.Sin(2 * math.Pi * float64(freqHz) * float64(i) / float64(rate))
		gain := 1.0
		if rampLen > 0 {
			if i < rampLen {
				gain = float64(i) / float64(rampLen)
			} else if i > total-rampLen {
				gain = float64(total-i) / float64(rampLen)
			}
		}
		s := int16(float64(amp) * v * gain)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

// BeepDone 是"说完了"提示音：下行双音（880 → 660）。
//
// 刻意用下行，和"收到"那声高音短叮（1200Hz）拉开区别：
// 用户需要能靠听分辨"它听见了"和"它说完了"，两者都响上行音就分不清。
func BeepDone(rate int) []byte {
	return append(TonePCM(rate, 880, 70, 0, 9000), TonePCM(rate, 660, 100, 0, 9000)...)
}

// BeepWake 是唤醒提示音：单声轻响。
func BeepWake(rate int) []byte {
	return TonePCM(rate, 1046, 80, 0, 7000)
}

// BeepError 是失败提示音：低音两下。
func BeepError(rate int) []byte {
	return append(TonePCM(rate, 440, 90, 0, 8000), TonePCM(rate, 330, 120, 0, 8000)...)
}
