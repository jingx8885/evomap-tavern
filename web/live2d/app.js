(() => {
  const MODEL = "/models/Haru/Haru.model3.json";
  const MODES = [
    "continue",
    "comfort",
    "de_escalate",
    "celebrate",
    "re_engage",
    "goal_push",
    "safety",
  ];
  const DEMO = {
    continue: { emotion: "neutral", self_emotion: "neutral", valence: 0.5, arousal: 0.4, engagement: 0.6, relationship_stage: "熟悉" },
    comfort: { emotion: "sadness", self_emotion: "anger", valence: 0.2, arousal: 0.3, engagement: 0.8, relationship_stage: "信任" },
    de_escalate: { emotion: "anger", self_emotion: "fear", valence: 0.3, arousal: 0.7, engagement: 0.7, relationship_stage: "熟悉" },
    celebrate: { emotion: "joy", self_emotion: "joy", valence: 0.9, arousal: 0.8, engagement: 0.9, relationship_stage: "亲密" },
    re_engage: { emotion: "neutral", self_emotion: "sadness", valence: 0.5, arousal: 0.4, engagement: 0.15, relationship_stage: "熟悉" },
    goal_push: { emotion: "neutral", self_emotion: "disgust", valence: 0.65, arousal: 0.55, engagement: 0.8, relationship_stage: "熟悉" },
    safety: { emotion: "fear", self_emotion: "fear", valence: 0.15, arousal: 0.6, engagement: 0.5, safety_p: 0.9, relationship_stage: "信任" },
  };
  const MOUTH_IDS = ["ParamMouthOpenY", "ParamA", "ParamI", "ParamU", "ParamE", "ParamO"];

  const statusEl = document.getElementById("status");
  const connEl = document.getElementById("conn");
  const hud = {
    relationship: document.getElementById("relationship"),
    selfEmotion: document.getElementById("self-emotion"),
    doing: document.getElementById("doing"),
    scene: document.getElementById("scene"),
    mouth: document.getElementById("mouth"),
    self: document.getElementById("self"),
    eye: document.getElementById("eye"),
  };
  const mouthBar = document.querySelector("#mouth-bar > span");
  const unmuteBtn = document.getElementById("unmute");
  const traceList = document.getElementById("trace-list");
  const traceMeta = document.getElementById("trace-meta");
  let traceStick = true;

  let app;
  let model;
  let lastMode = "";
  let lastExpression = "";
  let lastDrive = { params: {} };
  let mouthTarget = 0;
  let mouth = 0;
  let lastLipsyncAt = 0;
  let mouthParamHooked = false;

  function setStatus(text) {
    statusEl.textContent = text;
  }

  function logKind(text) {
    const m = /^\[([^\]]+)\]/.exec(text || "");
    if (!m) return "note";
    return m[1].split(/[\s~]/)[0];
  }

  function appendLog(at, text) {
    if (!traceList || !text) return;
    const empty = document.getElementById("trace-empty");
    if (empty) empty.remove();
    const li = document.createElement("li");
    const kind = logKind(text);
    li.dataset.kind = kind;
    if (/fail|error|warning|失败|报错/i.test(text)) li.classList.add("fault");
    const time = document.createElement("time");
    const d = at ? new Date(at) : new Date();
    time.dateTime = d.toISOString();
    time.textContent = d.toLocaleTimeString("zh-CN", { hour12: false });
    const span = document.createElement("span");
    span.textContent = text;
    li.append(time, span);
    const nearEnd = traceList.scrollHeight - traceList.scrollTop - traceList.clientHeight < 48;
    traceList.appendChild(li);
    while (traceList.children.length > 200) {
      traceList.removeChild(traceList.firstChild);
    }
    if (traceStick || nearEnd) traceList.scrollTop = traceList.scrollHeight;
    if (traceMeta) traceMeta.textContent = traceList.children.length + " 条";
  }

  function setConn(state, label) {
    connEl.className = state;
    connEl.textContent = label;
  }

  function fmt(n) {
    return Number.isFinite(n) ? n.toFixed(2) : "—";
  }

  function setMouthHUD(v) {
    if (hud.mouth) hud.mouth.textContent = fmt(v);
    if (mouthBar) mouthBar.style.width = Math.round(Math.max(0, Math.min(1, v)) * 100) + "%";
  }

  function applyLipsync(v) {
    const n = Number(v);
    mouthTarget = Number.isFinite(n) ? Math.max(0, Math.min(1, n)) : 0;
    lastLipsyncAt = performance.now();
    setMouthHUD(mouthTarget);
  }

  function coreModel() {
    return model && model.internalModel && model.internalModel.coreModel;
  }

  function expressionManager() {
    const im = model && model.internalModel;
    return im && im.motionManager && im.motionManager.expressionManager;
  }

  function setParam(core, id, value) {
    if (!core || !id) return;
    const v = Number(value);
    if (!Number.isFinite(v)) return;
    if (typeof core.setParameterValueById === "function") {
      core.setParameterValueById(id, v);
      return;
    }
    if (typeof core.addParameterValueById === "function") {
      core.addParameterValueById(id, v);
    }
  }

  function addParam(core, id, value) {
    if (!core || !id) return;
    const v = Number(value);
    if (!Number.isFinite(v) || v === 0) return;
    if (typeof core.addParameterValueById === "function") {
      core.addParameterValueById(id, v);
      return;
    }
    setParam(core, id, v);
  }

  function mouthIds() {
    const ids = ["ParamMouthOpenY"];
    const im = model && model.internalModel;
    const fromSettings = im && im.settings && im.settings.lipSyncIds;
    const fromMotion = im && im.motionManager && im.motionManager.lipSyncIds;
    for (const id of [].concat(fromSettings || [], fromMotion || [])) {
      if (id && !ids.includes(id)) ids.push(id);
    }
    return ids;
  }

  let hookMouth = null;
  let hookIndex = null;

  function writeMouthParam(v) {
    const core = coreModel();
    if (!core) return;
    for (const id of mouthIds()) setParam(core, id, v);
    if (typeof core.getParameterIndex === "function") {
      hookIndex = core.getParameterIndex("ParamMouthOpenY");
    }
    hookMouth = readMouthParam();
  }

  function readMouthParam() {
    const core = coreModel();
    if (!core) return null;
    if (typeof core.getParameterValueById === "function") {
      const v = core.getParameterValueById("ParamMouthOpenY");
      if (Number.isFinite(v)) return v;
    }
    return null;
  }

  function writeEmotionParams() {
    const core = coreModel();
    if (!core) return;
    const params = (lastDrive && lastDrive.params) || {};
    for (const [id, value] of Object.entries(params)) {
      if (MOUTH_IDS.includes(id)) continue;
      addParam(core, id, value);
    }
  }

  function applyFace() {
    writeEmotionParams();
    writeMouthParam(mouth);
  }

  function tickMouth() {
    if (lastLipsyncAt && performance.now() - lastLipsyncAt > 400) {
      mouthTarget = 0;
    }
    mouth += (mouthTarget - mouth) * 0.55;
    if (mouth < 0.012 && mouthTarget < 0.012) mouth = 0;
  }

  function renderHud(frame) {
    if (hud.relationship) {
      hud.relationship.textContent = frame.relationship_stage || "刚认识";
    }
    if (hud.selfEmotion) {
      hud.selfEmotion.textContent = frame.self_emotion || frame.emotion || "—";
    }
    if (hud.doing) {
      hud.doing.textContent = frame.mode === "goal_push" ? "翻小账本催进度" :
        frame.mode === "comfort" ? "嘴硬地陪着你" :
        frame.mode === "celebrate" ? "别扭地替你高兴" :
        frame.mode === "de_escalate" ? "把火气压回去" :
        frame.mode === "re_engage" ? "把话接回来" : "听你说话";
    }
    if (hud.scene) {
      hud.scene.textContent = frame.relationship_stage || frame.expression || "桌面边";
    }
    for (const btn of document.querySelectorAll("#modes button")) {
      btn.classList.toggle("active", btn.dataset.mode === frame.mode);
    }
  }

  function renderSelf(s) {
    if (!hud.self) return;
    if (!s || !s.live) {
      hud.self.textContent = "—";
      return;
    }
    const bits = [];
    if (s.who) bits.push(s.who);
    bits.push(s.live.voice || "—");
    if (s.live.mode) bits.push(s.live.mode);
    hud.self.textContent = bits.join(" · ");
    if (hud.eye) {
      const cam = (s.live && s.live.camera) || "";
      const scr = (s.live && s.live.screen) || "";
      hud.eye.textContent = cam || scr ? [cam, scr].filter(Boolean).join(" / ") : "—";
    }
  }

  async function pollSense() {
    try {
      const r = await fetch("/api/sense");
      if (!r.ok) return;
      renderSelf(await r.json());
    } catch (e) {}
  }

  function expressionIndex(name) {
    const em = expressionManager();
    if (!em || !name) return -1;
    if (typeof em.getExpressionIndex === "function") {
      const idx = em.getExpressionIndex(name);
      if (idx >= 0) return idx;
    }
    if (Array.isArray(em.definitions)) {
      return em.definitions.findIndex((d) => d && (d.Name === name || d.name === name));
    }
    return -1;
  }

  async function applyExpression(name) {
    if (!name || !model) return;
    if (name === lastExpression) return;
    const idx = expressionIndex(name);
    try {
      if (idx >= 0) {
        await model.expression(idx);
      } else {
        await model.expression(name);
      }
      lastExpression = name;
    } catch (err) {
      console.warn("expression", name, err);
    }
  }

  async function applyDrive(frame) {
    if (!frame || !model) return;
    lastDrive = frame;
    renderHud(frame);
    await applyExpression(frame.expression);
    const group = frame.motion_group || "Idle";
    const index = frame.motion_index || 0;
    const sameIdle = group === "Idle" && lastMode === frame.mode;
    if (!sameIdle) {
      try {
        await model.motion(group, index);
      } catch (err) {
        console.warn("motion", err);
      }
    }
    lastMode = frame.mode;
    const look = Number.isFinite(frame.look_at) ? frame.look_at : 0.6;
    const w = app.renderer.width;
    const h = app.renderer.height;
    const x = w * (0.5 + (look - 0.5) * 0.35);
    const y = h * (0.28 + (1 - look) * 0.12);
    model.focus(x, y);
    setStatus(`${frame.relationship_stage || "现在"} · ${frame.self_emotion || frame.emotion || "听着"}`);
  }

  function layoutModel() {
    if (!model || !app) return;
    const w = app.renderer.width;
    const h = app.renderer.height;
    const rail = document.getElementById("rail");
    const hudBox = document.getElementById("hud");
    const insetL = hudBox ? hudBox.getBoundingClientRect().width + 36 : 300;
    const insetR = rail ? rail.getBoundingClientRect().width + 36 : 420;
    const usable = Math.max(240, w - insetL - insetR);
    model.anchor.set(0.5, 0);
    model.scale.set(1);
    const scale = Math.min((usable * 0.92) / Math.max(model.width, 1), (h * 1.22) / Math.max(model.height, 1));
    model.scale.set(scale);
    model.x = insetL + usable * 0.5;
    model.y = h * -0.06;
  }

  let liveWS = null;
  let camVideo = null;
  let camStream = null;
  let eyeTimer = null;
  let eyesOn = true;
  let eyesBusy = false;

  function sendEye(dataURL) {
    const payload = JSON.stringify({ type: "eye", source: "camera", data: dataURL });
    if (liveWS && liveWS.readyState === 1) {
      liveWS.send(payload);
      return;
    }
    fetch("/api/eye", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: payload,
    }).catch(function () {});
  }

  function grabCamera() {
    if (!eyesOn || !camVideo || camVideo.videoWidth < 2) return;
    const max = 480;
    let w = camVideo.videoWidth;
    let h = camVideo.videoHeight;
    const scale = Math.min(1, max / Math.max(w, h));
    w = Math.max(1, Math.round(w * scale));
    h = Math.max(1, Math.round(h * scale));
    const canvas = document.getElementById("eye-canvas");
    if (!canvas) return;
    canvas.width = w;
    canvas.height = h;
    canvas.getContext("2d").drawImage(camVideo, 0, 0, w, h);
    sendEye(canvas.toDataURL("image/jpeg", 0.55));
  }

  function ensureEyeTick() {
    if (eyeTimer) return;
    eyeTimer = setInterval(function () {
      if (camVideo) grabCamera();
      else {
        clearInterval(eyeTimer);
        eyeTimer = null;
      }
    }, 2000);
  }

  function setCameraOn(on) {
    const box = document.getElementById("cam");
    const btn = document.getElementById("eye-cam-btn");
    if (box) box.classList.toggle("on", on);
    if (btn) {
      btn.classList.toggle("active", on);
      btn.textContent = on ? "关闭" : "打开";
    }
  }

  function stopCamera() {
    if (camStream) camStream.getTracks().forEach(function (t) { t.stop(); });
    camStream = null;
    camVideo = null;
    const video = document.getElementById("eye-cam");
    if (video) {
      video.pause();
      video.srcObject = null;
    }
    setCameraOn(false);
  }

  async function toggleCamera() {
    if (camVideo) {
      stopCamera();
      setStatus("camera off");
      return;
    }
    try {
      const stream = await navigator.mediaDevices.getUserMedia({
        video: { width: 640, height: 480 },
        audio: false,
      });
      const video = document.getElementById("eye-cam");
      video.srcObject = stream;
      await video.play();
      camVideo = video;
      camStream = stream;
      const track = stream.getVideoTracks()[0];
      if (track) {
        track.addEventListener("ended", function () {
          stopCamera();
        });
      }
      setCameraOn(true);
      ensureEyeTick();
      setStatus("camera on");
    } catch (err) {
      stopCamera();
      setStatus("camera " + err);
    }
  }

  function paintEyeSwitch(available) {
    const btn = document.getElementById("eye-loop-btn");
    if (!btn) return;
    if (available === false) {
      btn.disabled = true;
      btn.classList.remove("active");
      btn.textContent = "无观察";
      return;
    }
    btn.disabled = false;
    btn.classList.toggle("active", eyesOn);
    btn.textContent = eyesOn ? "观察开" : "观察关";
  }

  async function syncEyes() {
    if (eyesBusy) return;
    try {
      const r = await fetch("/api/eyes");
      if (!r.ok) return;
      const body = await r.json();
      if (body && body.available === false) {
        paintEyeSwitch(false);
        return;
      }
      if (body && typeof body.on === "boolean") {
        eyesOn = body.on;
        paintEyeSwitch(true);
        if (eyesOn && camVideo) ensureEyeTick();
      }
    } catch (e) {}
  }

  async function toggleEyes() {
    if (eyesBusy) return;
    const next = !eyesOn;
    eyesBusy = true;
    try {
      const r = await fetch("/api/eyes", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ on: next }),
      });
      if (!r.ok) {
        setStatus("观察开关失败 " + r.status);
        return;
      }
      const body = await r.json();
      eyesOn = !!(body && body.on);
      paintEyeSwitch(true);
      if (eyesOn && camVideo) ensureEyeTick();
      setStatus(eyesOn ? "观察开" : "观察关，后台不再打模型");
    } catch (err) {
      setStatus("观察开关 " + err);
    } finally {
      eyesBusy = false;
    }
  }

  function bindEyes() {
    const cam = document.getElementById("eye-cam-btn");
    if (cam) cam.addEventListener("click", function () { toggleCamera(); });
    const loop = document.getElementById("eye-loop-btn");
    if (loop) loop.addEventListener("click", function () { toggleEyes(); });
    syncEyes();
  }

  function connectWS() {
    let delay = 500;
    const open = () => {
      const proto = location.protocol === "https:" ? "wss" : "ws";
      const ws = new WebSocket(`${proto}://${location.host}/ws`);
      ws.binaryType = "arraybuffer";
      liveWS = ws;
      ws.onopen = () => {
        delay = 500;
        setConn("ok", "live");
      };
      ws.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          return;
        }
        try {
          const msg = JSON.parse(ev.data);
          if (msg && msg.type === "lipsync") {
            applyLipsync(msg.mouth);
            return;
          }
          if (msg && msg.type === "log") {
            appendLog(msg.at, msg.text);
            return;
          }
          applyDrive(msg);
        } catch (err) {
          console.warn(err);
        }
      };
      ws.onclose = () => {
        if (liveWS === ws) liveWS = null;
        setConn("bad", "retry");
        setTimeout(open, delay);
        delay = Math.min(delay * 1.6, 8000);
      };
      ws.onerror = () => ws.close();
    };
    open();
  }

  async function pushMode(mode) {
    const payload = Object.assign({ mode: mode }, DEMO[mode] || {});
    const res = await fetch("/drive", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!res.ok) {
      setStatus("drive failed " + res.status);
    }
  }

  function bindModes() {
    const box = document.getElementById("modes");
    for (const mode of MODES) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.dataset.mode = mode;
      btn.textContent = mode;
      btn.addEventListener("click", () => pushMode(mode));
      box.appendChild(btn);
    }
  }

  function hookModelUpdate() {
    const im = model && model.internalModel;
    if (!im) return;
    if (typeof im.on === "function") {
      im.on("beforeModelUpdate", () => {
        mouthParamHooked = true;
        applyFace();
      });
    }
    if (typeof im.update === "function") {
      const orig = im.update.bind(im);
      im.update = function (dt, now) {
        tickMouth();
        orig(dt, now);
        if (!mouthParamHooked) {
          applyFace();
          if (this.coreModel && typeof this.coreModel.update === "function") {
            this.coreModel.update();
          }
        }
      };
    }
  }

  async function main() {
    if (traceList) {
      traceList.addEventListener("scroll", function () {
        traceStick = traceList.scrollHeight - traceList.scrollTop - traceList.clientHeight < 48;
      });
    }
    connectWS();
    bindEyes();
    setInterval(syncEyes, 2000);
    if (!window.PIXI || !PIXI.live2d) {
      setStatus("Cubism / Pixi runtime missing (CDN blocked?)");
      setConn("bad", "no runtime");
      return;
    }
    const canvas = document.getElementById("stage");
    app = new PIXI.Application({
      view: canvas,
      resizeTo: window,
      backgroundColor: 0x14110e,
      antialias: true,
    });
    setStatus("loading Haru…");
    model = await PIXI.live2d.Live2DModel.from(MODEL);
    app.stage.addChild(model);
    layoutModel();
    hookModelUpdate();
    window.addEventListener("resize", layoutModel);
    model.on("hit", (areas) => {
      if (areas.includes("HitArea2") || areas.includes("HitAreaBody") || areas.includes("Body")) {
        model.motion("TapBody");
      }
    });
    bindModes();
    pollSense();
    setInterval(pollSense, 2000);
    if (unmuteBtn) unmuteBtn.classList.add("hidden");
    window.__avatar = {
      get mouth() { return mouth; },
      get target() { return mouthTarget; },
      get param() { return readMouthParam(); },
      get hookMouth() { return hookMouth; },
      get hookIndex() { return hookIndex; },
      get expression() { return lastExpression; },
      get drive() { return lastDrive; },
      get hooked() { return mouthParamHooked; },
      get lipSyncIds() { return mouthIds(); },
      setMouth(v) { applyLipsync(v); },
    };
    setStatus("waiting for Jev…");
  }

  main().catch((err) => {
    console.error(err);
    setStatus(String(err));
    setConn("bad", "error");
  });
})();
