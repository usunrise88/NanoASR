# Voice isolation: reference, benchmarks and what they decided

Research tooling behind `internal/tse`, voice isolation on
[Voxlock](https://huggingface.co/arbazsiddiqui/voxlock). No Go code imports it and
`make test` does not run it. `golden_voxlock.py` writes `testdata/golden/voxlock`, which
the Go port is checked against; the rest is the measurements that shaped the design,
kept so they can be re-run rather than remembered.

Python needs `numpy soundfile onnxruntime` and, for the golden, `voxlock==1.1.1`.

## The model

A 27M-parameter causal band-split RNN, fine-tuned from the REAL-TSE baseline on
LibriSpeech and AMI, Apache-2.0. Two graphs: `enroll.onnx` turns a voice sample into
the conditioning, `chunk*.onnx` turns audio into windowed time-domain frames and carries
an LSTM state. The STFT padding and the overlap-add live outside the graphs; the Go
port (`internal/tse/voxlock`) is a line-for-line port of voxlock 1.1.1's `core.py`.

The sample is exactly 3 seconds. The graphs declare the axis dynamic, but the
extractor's attention reshapes the context to the 298 frames of a 3-second sample, and
2, 2.9, 3.1 and 4 seconds all fail inside the graph.

## The golden

    python -I golden_voxlock.py .models/voxlock-int8@2026-10-12

The Go port matches it to 45 dB. The reference itself moves by as much between
onnxruntime builds — 1.27 against 1.31 agree to 47.7 dB on the int8 graph — so the
test's bound is 35 dB: well clear of a padding or alignment bug, which lands near 0.

## What the measurements decided

**Choosing the sample with CAM++.** Voxlock's own default is the loudest 3 seconds in
the first 15. On synthetic mixtures of LibriSpeech readers (0 dB, full overlap) the
sample whose CAM++ embedding is nearest the speaker's centroid raised the mean gain from
+12.1 to +14.0 dB SI-SDR and the worst case from −20.5 to −0.8 dB.

**A gate on the output.** Where the chosen voice is silent and the other speaks, the
extractor alone passed the other voice in 3 of 20 English pairs and 3 of 6 Russian
ones (less than 10 dB down). A gate that embeds each 1.5 s stretch of the output and
mutes the ones nearer another voice than the chosen one took the other voice to −37 dB
on average in English and −28 dB in Russian, with one English pair left at −6 dB; in
the server, where the gate also knows the chosen voice's turns and each turn starts
fresh (below), the worst pair is −29 dB.

**Relative, not a threshold.** On `ru-2spk.wav` — one speaker and the same speaker
pitched — the other voice's stretches scored 0.29–0.44 against the chosen sample and the
chosen voice's own 0.40–0.86: a 0.45 threshold let the other through and cut the chosen
one. Against each other they never crossed (0.55–0.83 for the other voice's own sample).

**A fresh state per turn.** Carried through a real Russian dialogue
(`dialog_bench.py`), the LSTM state let the chosen voice go after the other speaker had
talked: the first second of 17 of Dima's 52 turns and 7 of Masha's 31 came out more
than 6 dB down. Fed from the start of each turn (with 0–2 s of lead-in, all the same)
the onset came back whole; the server now cuts the recording at the chosen voice's
turns, and `TestEveryTurnComesBackWhole` holds it.

## End to end

`server_bench.py` against the server, on dialogues of two readers — A 8 s, B 7 s,
A 6 s with B over its last 3, B 6 s, A 7 s — each voice isolated (two threads of a
2.1 GHz Xeon):

| | Isolations | SI-SDR gain | Chosen voice | Other voice (median) | Time |
|---|---|---|---|---|---|
| English | 20 | +18.4 dB (worst +7.4) | +0.6 dB | −39 dB (worst −29) | 0.39x |
| Russian | 6 | +14.6 dB (worst +9.6) | +0.4 dB | −33 dB (worst −17) | 0.38x |

`dialog_bench.py` on 3.6 minutes of the Dialogs corpus: both speakers found, the other
one 29 and 49 dB down, the chosen one's correlation with the input in its own turns
0.996 and 0.999; 14 s to find the voices, 0.42–0.46x real time per extraction.
