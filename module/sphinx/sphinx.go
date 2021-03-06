//
// Copyright (c) 2019-present Codist <countstarlight@gmail.com>. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.
// Written by Codist <countstarlight@gmail.com>, March 2019
//

package sphinx

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/countstarlight/homo/cmd/webview/config"
	"github.com/countstarlight/homo/module/audio"
	"github.com/countstarlight/homo/module/baidu"
	"github.com/countstarlight/homo/module/nlu"
	"github.com/countstarlight/homo/module/view"
	"github.com/sirupsen/logrus"
	"github.com/xlab/pocketsphinx-go/sphinx"
	"github.com/xlab/portaudio-go/portaudio"
	"io/ioutil"
	"unicode"
	"unsafe"
)

const (
	samplesPerChannel = 512
	sampleRate        = 16000
	channels          = 1
	sampleFormat      = portaudio.PaInt16
)

type Listener struct {
	inSpeech   bool
	uttStarted bool
	dec        *sphinx.Decoder
}

func LoadCMUSphinx() {
	config.SphinxLoop.Add(1)
	// Init CMUSphinx
	cfg := sphinx.NewConfig(
		sphinx.HMMDirOption(config.HMMDirEn),
		sphinx.DictFileOption(config.DictFileEn),
		sphinx.LMFileOption(config.LMFileEn),
		sphinx.SampleRateOption(sampleRate),
	)
	//Specify output dir for RAW recorded sound files (s16le). Directory must exist.
	sphinx.RawLogDirOption(config.RawDir)(cfg)

	sphinx.LogFileOption(config.SphinxLogFile)(cfg)

	logrus.Info("开始加载 CMU PhocketSphinx...")
	logrus.Info("开始加载唤醒模型...")
	dec, err := sphinx.NewDecoder(cfg)
	if err != nil {
		logrus.Fatalf("sphinx.NewDecoder failed: %s", err.Error())
	}
	defer dec.Destroy()

	dec.SetRawDataSize(300000)

	l := &Listener{
		dec: dec,
	}

	var stream *portaudio.Stream
	errStr := portaudio.OpenDefaultStream(&stream, channels, 0, sampleFormat, sampleRate, samplesPerChannel, l.paCallback, nil)
	if audio.PaError(errStr) {
		logrus.Fatalf("PortAudio failed: %s", audio.PaErrorText(errStr))
	}
	defer func() {
		if errStr := portaudio.CloseStream(stream); audio.PaError(errStr) {
			logrus.Warnf("PortAudio error:", audio.PaErrorText(errStr))
		}
	}()
	if errStr := portaudio.StartStream(stream); audio.PaError(errStr) {
		logrus.Fatalf("PortAudio error: %s", audio.PaErrorText(errStr))
	}
	defer func() {
		if errStr := portaudio.StopStream(stream); audio.PaError(errStr) {
			logrus.Fatalf("PortAudio error:", audio.PaErrorText(errStr))
		}
	}()
	if !dec.StartUtt() {
		logrus.Fatalln("Sphinx failed to start utterance")
	}
	logrus.Infof("开始从麦克风检测唤醒词：采样率[%dHz] 通道数[%d]", sampleRate, channels)
	config.SphinxLoop.Wait()
}

// paCallback: for simplicity reasons we process raw audio with sphinx in the this stream callback,
// never do that for any serious applications, use a buffered channel instead.
func (l *Listener) paCallback(input unsafe.Pointer, _ unsafe.Pointer, sampleCount uint,
	_ *portaudio.StreamCallbackTimeInfo, _ portaudio.StreamCallbackFlags, _ unsafe.Pointer) int32 {

	const (
		statusContinue = int32(portaudio.PaContinue)
		statusAbort    = int32(portaudio.PaAbort)
	)

	// Do not record if is playing voice
	if config.IsPlayingVoice && !config.InterruptMode {
		return statusContinue
	}

	in := (*(*[1 << 24]int16)(input))[:int(sampleCount)*channels]
	// ProcessRaw with disabled search because callback needs to be relatime
	_, ok := l.dec.ProcessRaw(in, true, false)
	// log.Printf("processed: %d frames, ok: %v", frames, ok)
	if !ok {
		return statusAbort
	}

	if l.dec.IsInSpeech() {
		l.inSpeech = true
		if !l.uttStarted {
			l.uttStarted = true
			if config.WakeUpd {
				logrus.Info("发现音频波动，开始录制音频...")
			} else {
				logrus.Info("发现音频波动，开始检测唤醒词...")
			}
		}
	} else if l.uttStarted {
		// speech -> silence transition, time to start new utterance
		l.dec.EndUtt()
		l.uttStarted = false
		l.report() // report results
		if !l.dec.StartUtt() {
			logrus.Fatalln("Sphinx failed to start utterance")
		}
	}
	return statusContinue
}

func (l *Listener) report() {
	if config.WakeUpd {
		//Display typing animate during asr
		if !config.SilenceMode {
			view.TypingAnimate()
		}

		// Save raw data to file
		rData := l.dec.RawData()

		buf := new(bytes.Buffer)
		for _, v := range rData {
			err := binary.Write(buf, binary.LittleEndian, v)
			if err != nil {
				logrus.Errorf("binary.Write failed: %s", err.Error())
			}
		}

		//Reduce sensitivity
		if len(buf.Bytes()) < config.RecordThreshold {
			logrus.Infof("音频长度小于阈值 %d，取消录制", config.RecordThreshold)
			view.TypingAnimateStop()
			return
		}

		logrus.Infof("保存原始音频文件到: %s, 音频流长度: %d Byte\n", config.InputRaw, len(buf.Bytes()))

		if err := ioutil.WriteFile(config.InputRaw, buf.Bytes(), 0644); err != nil {
			logrus.Warnf("binary.Write failed: %s", err.Error())
		}

		// Speech to text
		success := false
		var errorMsg string
		result, err := baidu.SpeechToText(config.InputRaw, "pcm", sampleRate)
		if err != nil {
			if baidu.IsErrSpeechQuality(err) {
				view.TypingAnimateStop()
				// errorMsg = "没有听清在说什么"
				return
				//result = []string{"没有听清在说什么"}
				//logrus.Warnf("没有听清在说什么")
			} else {
				errorMsg = fmt.Sprintf("语音在线识别出错：%s", err.Error())
				//logrus.Warnf("语音在线识别出错：%s", err.Error())
			}
		} else {
			if len(result) == 0 {
				view.TypingAnimateStop()
				// errorMsg = "没有听清在说什么"
				return
				//result = []string{"没有听清在说什么"}
				//logrus.Warnf("没有听清在说什么")
			} else {
				success = true
				//logrus.Infof("语音在线识别结果: %v", result)
			}
		}

		if !success {
			//Output message to log if in silence mode
			if config.SilenceMode {
				logrus.Warnf("%s", errorMsg)
			} else {
				/*if !config.OfflineMode {
					view.SendReplyWithVoice([]string{errorMsg})
				} else {
					view.SendReply([]string{errorMsg})
				}*/
				//error message not to speech
				view.SendReply([]string{errorMsg})
			}
		} else {
			// Send input text to webview
			var message string
			s := []rune(result[0])

			// Remove latest symbol of Chinese string, eg. '。' of '你好。'
			if !unicode.Is(unicode.Han, s[len(s)-1]) {
				message = string(s[:len(s)-1])
			}
			// If in silence mode, post message to nlu
			if config.SilenceMode {
				replyMessage, err := nlu.ActionLocal(message)
				if err != nil {
					logrus.Warnf("连接到nlu出错: %s", err.Error())
				}
				if !config.SilenceMode {
					view.SendOnlyInputText(message)
					if config.OfflineMode {
						view.SendReply(replyMessage)
					} else {
						view.SendReplyWithVoice(replyMessage)
					}
				}
			} else {
				view.SendInputText(message)
			}
		}

		if config.RawToWav {
			err := Pcm2Wav(config.InputRaw)
			if err != nil {
				logrus.Warnf("Convert raw input %s to wav failed: %s", config.InputRaw, err.Error())
			} else {
				logrus.Infof("原始音频文件编码为wav，保存到: %s", config.InputWav)
			}
		}
	} else {
		hyp, _ := l.dec.Hypothesis()
		if len(hyp) > 0 {
			if hyp == "homo" || hyp == "como" {
				logrus.Info("命中唤醒词，开始唤醒...")
				config.WakeUpWait.Done()
				config.WakeUpd = true
			}
			return
		}
		logrus.Println("没有检测到唤醒词")
	}
}
