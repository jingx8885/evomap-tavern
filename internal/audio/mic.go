//go:build !tavern_mic

package audio

// OpenMic in the default build returns a nil channel plus ErrNoMicDevice;
// the caller substitutes the silence source so uplink RTP keeps flowing
// (the gateway only plays speakable text while uplink RTP is active).
// Real capture is compiled with -tags tavern_mic.
func OpenMic(sampleRate int) (<-chan []byte, func(), error) {
	return nil, func() {}, ErrNoMicDevice
}
