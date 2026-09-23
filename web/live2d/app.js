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
  let lastMotion = "";
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

  let pendingDrive = null;

  function paintStatus(frame) {
    setStatus(`${frame.relationship_stage || "现在"} · ${frame.self_emotion || frame.emotion || "听着"}`);
  }

  async function applyDrive(frame) {
    if (!frame || frame.type === "lipsync" || frame.type === "log" || frame.type === "capture" || frame.type === "shot") {
      return;
    }
    // The socket often delivers the startup face before Cubism finishes
    // loading. Keep it and paint once the model exists.
    pendingDrive = frame;
    if (!model) return;
    lastDrive = frame;
    renderHud(frame);
    // Haru's idle motions loop forever. Waiting for model.motion() to
    // settle meant this line stayed on "waiting for Jev" after every turn.
    paintStatus(frame);
    lastMode = frame.mode;
    const look = Number.isFinite(frame.look_at) ? frame.look_at : 0.6;
    const w = app.renderer.width;
    const h = app.renderer.height;
    const x = w * (0.5 + (look - 0.5) * 0.35);
    const y = h * (0.28 + (1 - look) * 0.12);
    model.focus(x, y);
    void applyExpression(frame.expression);
    const group = frame.motion_group || "Idle";
    const index = Number.isFinite(frame.motion_index) ? frame.motion_index : 0;
    const motionKey = group + ":" + index;
    if (motionKey !== lastMotion) {
      lastMotion = motionKey;
      try {
        const started = model.motion(group, index);
        if (started && typeof started.catch === "function") {
          started.catch((err) => console.warn("motion", group, index, err));
        }
      } catch (err) {
        console.warn("motion", group, index, err);
      }
    }
  }

  const phoneQuery = window.matchMedia("(max-width: 820px), (max-height: 500px) and (max-width: 1024px)");

  function phoneLayout() {
    return phoneQuery.matches;
  }

  function layoutModel() {
    if (!model || !app) return;
    const w = app.renderer.width;
    const h = app.renderer.height;
    model.anchor.set(0.5, 0);
    model.scale.set(1);
    if (phoneLayout()) {
      document.body.classList.add("phone");
      const mw = Math.max(model.width, 1);
      const mh = Math.max(model.height, 1);
      const scale = Math.min((w * 1.28) / mw, (h * 1.18) / mh);
      model.scale.set(scale);
      model.x = w * 0.5;
      model.y = h * -0.02;
      return;
    }
    document.body.classList.remove("phone");
    const rail = document.getElementById("rail");
    const hudBox = document.getElementById("hud");
    const insetL = hudBox ? hudBox.getBoundingClientRect().width + 36 : 300;
    const insetR = rail ? rail.getBoundingClientRect().width + 36 : 420;
    const usable = Math.max(240, w - insetL - insetR);
    const scale = Math.min((usable * 0.92) / Math.max(model.width, 1), (h * 1.22) / Math.max(model.height, 1));
    model.scale.set(scale);
    model.x = insetL + usable * 0.5;
    model.y = h * -0.06;
  }

  let liveWS = null;
  let camVideo = null;
  let camStream = null;

  function sendEye(source, dataURL) {
    const payload = JSON.stringify({ type: "eye", source: source, data: dataURL });
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
    if (!camVideo || camVideo.videoWidth < 2) return;
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
    sendEye("camera", canvas.toDataURL("image/jpeg", 0.55));
  }

  function grabSelf() {
    const src = document.getElementById("stage");
    if (!src || src.width < 2 || src.height < 2) return;
    const max = 720;
    let w = src.width;
    let h = src.height;
    const scale = Math.min(1, max / Math.max(w, h));
    w = Math.max(1, Math.round(w * scale));
    h = Math.max(1, Math.round(h * scale));
    const canvas = document.createElement("canvas");
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.drawImage(src, 0, 0, w, h);
    sendEye("shot", canvas.toDataURL("image/jpeg", 0.6));
  }

  const FOLD_KEY = "lov-evo-panel-fold";
  const foldDefault = { cam: true };

  function readFolds() {
    try {
      const raw = JSON.parse(localStorage.getItem(FOLD_KEY) || "{}");
      return raw && typeof raw === "object" ? raw : {};
    } catch (err) {
      return {};
    }
  }

  function setFold(panel, folded, persist) {
    if (!panel) return;
    panel.classList.toggle("folded", !!folded);
    const btn = panel.querySelector(":scope > header .fold");
    if (btn) btn.setAttribute("aria-expanded", folded ? "false" : "true");
    if (!persist || !panel.id) return;
    const saved = readFolds();
    saved[panel.id] = !!folded;
    localStorage.setItem(FOLD_KEY, JSON.stringify(saved));
  }

  function bindFolds() {
    const saved = readFolds();
    document.querySelectorAll(".panel").forEach(function (panel) {
      const known = Object.prototype.hasOwnProperty.call(saved, panel.id);
      setFold(panel, known ? !!saved[panel.id] : !!foldDefault[panel.id], false);
      const header = panel.querySelector(":scope > header");
      if (!header) return;
      header.addEventListener("click", function (ev) {
        const other = ev.target.closest("button, a, input, select, textarea");
        if (other && !other.classList.contains("fold")) return;
        setFold(panel, !panel.classList.contains("folded"), true);
      });
    });
  }

  function setCameraOn(on) {
    const box = document.getElementById("cam");
    const btn = document.getElementById("eye-cam-btn");
    if (box) box.classList.toggle("on", on);
    if (on) setFold(box, false, true);
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
      setStatus("camera on");
    } catch (err) {
      stopCamera();
      setStatus("camera " + err);
    }
  }

  function bindEyes() {
    const cam = document.getElementById("eye-cam-btn");
    if (cam) cam.addEventListener("click", function () { toggleCamera(); });
  }

  let systemOn = true;
  let systemBusy = false;

  function paintPower(available) {
    const btn = document.getElementById("power-btn");
    if (!btn) return;
    if (available === false) {
      btn.disabled = true;
      btn.classList.remove("on");
      btn.classList.add("off");
      btn.textContent = "未接线";
      return;
    }
    btn.disabled = false;
    btn.classList.toggle("on", systemOn);
    btn.classList.toggle("off", !systemOn);
    btn.textContent = systemOn ? "运行中" : "已暂停";
  }

  async function syncPower() {
    if (systemBusy) return;
    try {
      const r = await fetch("/api/system");
      if (!r.ok) return;
      const body = await r.json();
      if (body && body.available === false) {
        paintPower(false);
        return;
      }
      if (body && typeof body.on === "boolean") {
        systemOn = body.on;
        paintPower(true);
      }
    } catch (e) {}
  }

  async function togglePower() {
    if (systemBusy) return;
    const next = !systemOn;
    systemBusy = true;
    try {
      const r = await fetch("/api/system", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ on: next }),
      });
      if (!r.ok) {
        setStatus("总开关失败 " + r.status);
        return;
      }
      const body = await r.json();
      systemOn = !!(body && body.on);
      paintPower(true);
      setStatus(systemOn ? "运行中" : "已暂停，语音和判断都停了");
    } catch (err) {
      setStatus("总开关 " + err);
    } finally {
      systemBusy = false;
    }
  }

  function bindPower() {
    const btn = document.getElementById("power-btn");
    if (btn) btn.addEventListener("click", function () { togglePower(); });
    syncPower();
    setInterval(syncPower, 2000);
  }

  const MEMORY_LABEL = {
    open_loop: "还记着",
    promise: "答应",
    shared_event: "经历",
    inside_joke: "梗",
    topic: "话题",
  };
  let memoryEditing = "";
  let memoryQuiet = false;
  let memoryStamp = "";
  let memoryGen = 0;

  function memoryLocked() {
    if (memoryEditing || memoryQuiet) return true;
    const root = document.getElementById("memory");
    return !!(root && document.activeElement && root.contains(document.activeElement));
  }

  function paintMemoryError(text) {
    memoryStamp = "";
    const meta = document.getElementById("memory-meta");
    if (!meta) return;
    meta.classList.add("bad");
    meta.textContent = text || "没记住";
  }

  function memorySignature(view) {
    const items = (view && view.items) || [];
    return JSON.stringify({
      available: !!(view && view.available),
      stage: (view && view.stage) || "",
      summary: (view && view.summary) || "",
      editing: memoryEditing,
      items: items.map(function (item) {
        return (item.id || "") + "\n" + (item.kind || "") + "\n" + (item.text || "") + "\n" + (item.weight || 0) + "\n" + (item.status || "");
      }),
    });
  }

  function paintMemory(view) {
    const meta = document.getElementById("memory-meta");
    const summary = document.getElementById("memory-summary");
    const list = document.getElementById("memory-list");
    if (!list || !meta) return;
    view = view || {};
    const stamp = memorySignature(view);
    if (stamp === memoryStamp) return;
    memoryStamp = stamp;
    meta.classList.remove("bad");
    if (!view.available) {
      meta.textContent = "未接线";
      if (summary) summary.textContent = "";
      list.innerHTML = "";
      const empty = document.createElement("li");
      empty.className = "mem-empty";
      empty.textContent = "这次运行没有记忆文件。";
      list.appendChild(empty);
      return;
    }
    const items = view.items || [];
    meta.textContent = (view.stage || "—") + " · " + items.length + " 条";
    if (summary) summary.textContent = view.summary || "";
    list.innerHTML = "";
    if (!items.length) {
      const empty = document.createElement("li");
      empty.className = "mem-empty";
      empty.textContent = "她还没记下什么。";
      list.appendChild(empty);
      return;
    }
    items.forEach(function (item) {
      const li = document.createElement("li");
      const kind = document.createElement("span");
      kind.className = "mem-kind";
      if (item.status === "fading") kind.classList.add("fading");
      kind.textContent = MEMORY_LABEL[item.kind] || item.kind;
      kind.title = item.status === "fading" ? "在淡" : "";
      const text = document.createElement("div");
      text.className = "mem-text";
      const actions = document.createElement("div");
      actions.className = "mem-actions";
      if (memoryEditing === item.id) {
        const input = document.createElement("input");
        input.value = item.text || "";
        input.maxLength = 80;
        text.appendChild(input);
        const save = document.createElement("button");
        save.type = "button";
        save.textContent = "存";
        save.addEventListener("click", function () {
          postMemory({ op: "update", id: item.id, text: input.value });
        });
        const cancel = document.createElement("button");
        cancel.type = "button";
        cancel.textContent = "取消";
        cancel.addEventListener("click", function () {
          memoryEditing = "";
          syncMemory();
        });
        actions.append(save, cancel);
        li.append(kind, text, actions);
        list.appendChild(li);
        input.focus();
        return;
      }
      text.textContent = "";
      const label = document.createElement("span");
      label.textContent = item.text || "";
      text.appendChild(label);
      if (typeof item.weight === "number" && item.weight > 0) {
        const bar = document.createElement("i");
        bar.className = "mem-weight";
        if (item.status === "fading") bar.classList.add("fading");
        bar.style.width = Math.max(8, Math.round(item.weight * 100)) + "%";
        text.appendChild(bar);
      }
      const edit = document.createElement("button");
      edit.type = "button";
      edit.textContent = "改";
      edit.addEventListener("click", function () {
        memoryEditing = item.id;
        paintMemory(view);
      });
      const del = document.createElement("button");
      del.type = "button";
      del.textContent = "删";
      del.addEventListener("click", function () {
        if (del.dataset.confirm !== "1") {
          del.dataset.confirm = "1";
          del.classList.add("danger");
          del.textContent = "确定";
          memoryQuiet = true;
          setTimeout(function () {
            if (del.dataset.confirm === "1") {
              del.dataset.confirm = "";
              del.classList.remove("danger");
              del.textContent = "删";
              memoryQuiet = false;
            }
          }, 2500);
          return;
        }
        memoryQuiet = false;
        postMemory({ op: "delete", id: item.id });
      });
      actions.append(edit, del);
      li.append(kind, text, actions);
      list.appendChild(li);
    });
  }

  async function postMemory(op) {
    const gen = ++memoryGen;
    try {
      const r = await fetch("/api/memory", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(op),
      });
      const body = await r.json().catch(function () { return {}; });
      if (gen !== memoryGen) return;
      if (!r.ok) {
        paintMemoryError((body && body.error) || ("没记住 " + r.status));
        return;
      }
      memoryEditing = "";
      memoryQuiet = false;
      paintMemory(body);
    } catch (err) {
      if (gen === memoryGen) paintMemoryError(String(err));
    }
  }

  async function syncMemory() {
    if (memoryLocked()) return;
    const gen = memoryGen;
    try {
      const r = await fetch("/api/memory");
      if (!r.ok || gen !== memoryGen) return;
      paintMemory(await r.json());
    } catch (e) {}
  }

  const QUEUE_KIND = {
    image: "图",
    video: "视频",
    speech: "语音",
    song: "歌",
    llm: "笔记",
    codex: "编程",
  };
  const QUEUE_STATUS = {
    queued: "排队",
    running: "在做",
    ready: "好了",
    failed: "失败",
    canceled: "取消",
  };
  let queueStamp = "";

  function queueSignature(view) {
    const items = (view && view.jobs) || [];
    return JSON.stringify({
      available: !!(view && view.available),
      open: !!(view && view.open),
      feature: (view && view.feature) || "",
      media: (view && view.media) || "",
      jobs: items.map(function (job) {
        return [
          job.id, job.kind, job.status, job.ratio, job.prompt,
          job.file, job.text, job.err, job.look,
        ].join("\n");
      }),
    });
  }

  function queueMedia(view, file) {
    const base = String((view && view.media) || "").replace(/\/$/, "");
    if (!file || !/^https?:\/\//i.test(base)) return "";
    return base + "/media/" + encodeURIComponent(file);
  }

  function paintQueue(view) {
    const meta = document.getElementById("queue-meta");
    const list = document.getElementById("queue-list");
    if (!meta || !list) return;
    view = view || {};
    const stamp = queueSignature(view);
    if (stamp === queueStamp) return;
    queueStamp = stamp;
    const jobs = view.jobs || [];
    if (!view.available) {
      meta.textContent = "未接线";
      list.innerHTML = "";
      const empty = document.createElement("li");
      empty.className = "q-empty";
      empty.textContent = "这次运行没有任务队列。";
      list.appendChild(empty);
      return;
    }
    const running = jobs.filter(function (job) { return job.status === "running"; }).length;
    const where = view.open ? "开着" : "合上";
    if (!jobs.length) {
      meta.textContent = where + " · 空";
    } else if (running) {
      meta.textContent = where + " · " + running + " 在做";
    } else {
      meta.textContent = where + " · " + jobs.length + " 条";
    }
    list.innerHTML = "";
    if (!jobs.length) {
      const empty = document.createElement("li");
      empty.className = "q-empty";
      empty.textContent = "队列是空的。";
      list.appendChild(empty);
      return;
    }
    jobs.forEach(function (job) {
      const li = document.createElement("li");
      li.className = job.status || "";
      if (view.feature && job.id === view.feature) li.classList.add("on");
      const head = document.createElement("div");
      head.className = "q-head";
      const kind = document.createElement("span");
      kind.className = "q-kind";
      kind.textContent = QUEUE_KIND[job.kind] || job.kind || "任务";
      const status = document.createElement("span");
      status.className = "q-status";
      let label = QUEUE_STATUS[job.status] || job.status || "";
      if (job.status === "running" && typeof job.ratio === "number") {
        label += " " + Math.round(Math.max(0, Math.min(1, job.ratio)) * 100) + "%";
      }
      status.textContent = label;
      head.append(kind, status);
      const prompt = document.createElement("div");
      prompt.className = "q-prompt";
      prompt.textContent = job.prompt || "";
      li.append(head, prompt);
      const ratio = Math.max(0, Math.min(1, job.ratio || 0));
      const bar = document.createElement("div");
      bar.className = "q-bar";
      const fill = document.createElement("i");
      fill.style.width = Math.round(ratio * 100) + "%";
      bar.appendChild(fill);
      li.appendChild(bar);
      const src = queueMedia(view, job.file);
      if (src && job.kind === "image") {
        const img = document.createElement("img");
        img.className = "q-media";
        img.alt = "";
        img.src = src;
        li.appendChild(img);
      } else if (src && job.kind === "video") {
        const video = document.createElement("video");
        video.className = "q-media";
        video.controls = true;
        video.src = src;
        li.appendChild(video);
      } else if (src && (job.kind === "speech" || job.kind === "song")) {
        const audio = document.createElement("audio");
        audio.className = "q-media audio";
        audio.controls = true;
        audio.src = src;
        li.appendChild(audio);
      }
      if (job.text) {
        const note = document.createElement("div");
        note.className = "q-note";
        note.textContent = job.text;
        li.appendChild(note);
      }
      if (job.err && job.status === "failed") {
        const err = document.createElement("div");
        err.className = "q-err";
        err.textContent = job.err;
        li.appendChild(err);
      }
      list.appendChild(li);
    });
  }

  async function syncQueue() {
    try {
      const r = await fetch("/api/queue");
      if (!r.ok) {
        paintQueue({ available: false, jobs: [] });
        return;
      }
      paintQueue(await r.json());
    } catch (e) {
      paintQueue({ available: false, jobs: [] });
    }
  }

  function bindQueue() {
    syncQueue();
    setInterval(syncQueue, 2000);
  }

  function bindMemory() {
    const form = document.getElementById("memory-add");
    if (form) {
      form.addEventListener("submit", function (ev) {
        ev.preventDefault();
        const kind = document.getElementById("memory-kind");
        const text = document.getElementById("memory-text");
        const value = text ? text.value : "";
        postMemory({ op: "add", kind: kind ? kind.value : "", text: value }).then(function () {
          if (text && !document.getElementById("memory-meta").classList.contains("bad")) {
            text.value = "";
          }
        });
      });
    }
    syncMemory();
    setInterval(syncMemory, 2000);
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
          if (msg && msg.type === "capture") {
            grabCamera();
            return;
          }
          if (msg && msg.type === "shot") {
            grabSelf();
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
    bindFolds();
    if (traceList) {
      traceList.addEventListener("scroll", function () {
        traceStick = traceList.scrollHeight - traceList.scrollTop - traceList.clientHeight < 48;
      });
    }
    connectWS();
    bindEyes();
    bindPower();
    bindQueue();
    bindMemory();
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
      preserveDrawingBuffer: true,
    });
    setStatus("loading Haru…");
    model = await PIXI.live2d.Live2DModel.from(MODEL);
    app.stage.addChild(model);
    layoutModel();
    hookModelUpdate();
    window.addEventListener("resize", layoutModel);
    if (typeof phoneQuery.addEventListener === "function") {
      phoneQuery.addEventListener("change", layoutModel);
    }
    if (window.visualViewport) {
      window.visualViewport.addEventListener("resize", layoutModel);
    }
    model.on("hit", (areas) => {
      if (areas.includes("HitArea2") || areas.includes("HitAreaBody") || areas.includes("Body")) {
        model.motion("TapBody");
      }
    });
    bindModes();
    pollSense();
    setInterval(pollSense, 2000);
    if (unmuteBtn) unmuteBtn.classList.add("hidden");
    if (pendingDrive) {
      await applyDrive(pendingDrive);
    }
    if (!lastDrive.mode) {
      try {
        const last = await fetch("/api/last");
        if (last.ok) {
          const frame = await last.json();
          if (frame && frame.mode) await applyDrive(frame);
        }
      } catch (err) {
        console.warn("last drive", err);
      }
    }
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
    if (!lastDrive.mode) setStatus("听着");
  }

  main().catch((err) => {
    console.error(err);
    setStatus(String(err));
    setConn("bad", "error");
  });
})();
