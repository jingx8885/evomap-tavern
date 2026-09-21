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
    continue: { emotion: "neutral", valence: 0.5, arousal: 0.4, engagement: 0.6 },
    comfort: { emotion: "sadness", valence: 0.2, arousal: 0.3, engagement: 0.8 },
    de_escalate: { emotion: "anger", valence: 0.3, arousal: 0.7, engagement: 0.7 },
    celebrate: { emotion: "joy", valence: 0.9, arousal: 0.8, engagement: 0.9 },
    re_engage: { emotion: "neutral", valence: 0.5, arousal: 0.4, engagement: 0.15 },
    goal_push: { emotion: "neutral", valence: 0.65, arousal: 0.55, engagement: 0.8 },
    safety: { emotion: "fear", valence: 0.15, arousal: 0.6, engagement: 0.5, safety_p: 0.9 },
  };
  const MOUTH_IDS = ["ParamMouthOpenY", "ParamA", "ParamI", "ParamU", "ParamE", "ParamO"];

  const statusEl = document.getElementById("status");
  const connEl = document.getElementById("conn");
  const hud = {
    mode: document.getElementById("mode"),
    emotion: document.getElementById("emotion"),
    expression: document.getElementById("expression"),
    valence: document.getElementById("valence"),
    arousal: document.getElementById("arousal"),
    engagement: document.getElementById("engagement"),
    mouth: document.getElementById("mouth"),
  };
  const mouthBar = document.querySelector("#mouth-bar > span");
  const unmuteBtn = document.getElementById("unmute");

  let app;
  let model;
  let lastMode = "";
  let lastDrive = { params: {} };
  let mouthTarget = 0;
  let mouth = 0;
  let lastLipsyncAt = 0;

  function setStatus(text) {
    statusEl.textContent = text;
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

  function writeMouthParam(v) {
    const im = model && model.internalModel;
    const core = im && im.coreModel;
    if (!core) return;
    if (im) {
      im.lipSync = true;
      im.lipSyncValue = v;
    }
    const ids = ["ParamMouthOpenY", "ParamA"];
    if (im.settings && Array.isArray(im.settings.lipSyncIds)) {
      for (const id of im.settings.lipSyncIds) ids.push(id);
    }
    for (const id of ids) setParam(core, id, v);
    if (typeof core.getParameterIndex === "function") {
      const idx = core.getParameterIndex("ParamMouthOpenY");
      if (idx >= 0 && typeof core.setParameterValueByIndex === "function") {
        core.setParameterValueByIndex(idx, v);
      }
    }
  }

  function readMouthParam() {
    const core = coreModel();
    if (!core) return null;
    if (typeof core.getParameterValueById === "function") {
      const v = core.getParameterValueById("ParamMouthOpenY");
      if (Number.isFinite(v)) return v;
    }
    if (typeof core.getParameterIndex === "function" && typeof core.getParameterValueByIndex === "function") {
      const idx = core.getParameterIndex("ParamMouthOpenY");
      if (idx >= 0) return core.getParameterValueByIndex(idx);
    }
    return null;
  }

  function writeEmotionParams() {
    const core = coreModel();
    if (!core) return;
    const params = (lastDrive && lastDrive.params) || {};
    for (const [id, value] of Object.entries(params)) {
      if (MOUTH_IDS.includes(id)) continue;
      setParam(core, id, value);
    }
  }

  function renderHud(frame) {
    hud.mode.textContent = frame.mode || "—";
    hud.emotion.textContent = frame.emotion || "—";
    hud.expression.textContent = frame.expression || "—";
    hud.valence.textContent = fmt(frame.valence);
    hud.arousal.textContent = fmt(frame.arousal);
    hud.engagement.textContent = fmt(frame.engagement);
    for (const btn of document.querySelectorAll("#modes button")) {
      btn.classList.toggle("active", btn.dataset.mode === frame.mode);
    }
  }

  async function applyDrive(frame) {
    if (!frame || !model) return;
    lastDrive = frame;
    renderHud(frame);
    try {
      if (frame.expression) {
        await model.expression(frame.expression);
      }
    } catch (err) {
      console.warn("expression", err);
    }
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
    setStatus(`mode ${frame.mode} · ${frame.expression}`);
  }

  function layoutModel() {
    if (!model || !app) return;
    const w = app.renderer.width;
    const h = app.renderer.height;
    model.anchor.set(0.5, 0);
    model.scale.set(1);
    const scale = Math.min((w * 0.78) / Math.max(model.width, 1), (h * 1.22) / Math.max(model.height, 1));
    model.scale.set(scale);
    model.x = w * 0.57;
    model.y = h * -0.06;
  }

  function connectWS() {
    let delay = 500;
    const open = () => {
      const proto = location.protocol === "https:" ? "wss" : "ws";
      const ws = new WebSocket(`${proto}://${location.host}/ws`);
      ws.binaryType = "arraybuffer";
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
          applyDrive(msg);
        } catch (err) {
          console.warn(err);
        }
      };
      ws.onclose = () => {
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

  async function main() {
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
    const im = model.internalModel;
    if (im) {
      im.lipSync = true;
      im.lipSyncValue = 0;
    }
    if (im && typeof im.update === "function") {
      const orig = im.update.bind(im);
      im.update = function (dt, now) {
        if (lastLipsyncAt && performance.now() - lastLipsyncAt > 180) {
          mouthTarget = 0;
        }
        mouth += (mouthTarget - mouth) * 0.55;
        if (mouth < 0.012 && mouthTarget < 0.012) mouth = 0;
        this.lipSync = true;
        this.lipSyncValue = mouth;
        orig(dt, now);
        writeEmotionParams();
        writeMouthParam(mouth);
      };
    }
    window.addEventListener("resize", layoutModel);
    model.on("hit", (areas) => {
      if (areas.includes("HitArea2") || areas.includes("HitAreaBody") || areas.includes("Body")) {
        model.motion("TapBody");
      }
    });
    bindModes();
    if (unmuteBtn) unmuteBtn.classList.add("hidden");
    window.__avatar = {
      get mouth() { return mouth; },
      get target() { return mouthTarget; },
      get param() { return readMouthParam(); },
      get lipSyncIds() {
        const im = model && model.internalModel;
        return im && im.settings && im.settings.lipSyncIds;
      },
      get coreKeys() {
        const core = coreModel();
        if (!core) return [];
        return Object.getOwnPropertyNames(Object.getPrototypeOf(core) || {}).concat(Object.keys(core));
      },
    };
    connectWS();
    setStatus("waiting for Jev…");
  }

  main().catch((err) => {
    console.error(err);
    setStatus(String(err));
    setConn("bad", "error");
  });
})();
