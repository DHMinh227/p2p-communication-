"use strict";

const elements = {
  welcome: document.querySelector("#welcomeView"),
  waiting: document.querySelector("#waitingView"),
  chat: document.querySelector("#chatView"),
  name: document.querySelector("#displayName"),
  codeInput: document.querySelector("#roomCode"),
  create: document.querySelector("#createButton"),
  join: document.querySelector("#joinButton"),
  error: document.querySelector("#welcomeError"),
  waitingCode: document.querySelector("#waitingCode"),
  copyCode: document.querySelector("#copyCodeButton"),
  copyLink: document.querySelector("#copyLinkButton"),
  waitingStatus: document.querySelector("#waitingStatus"),
  cancel: document.querySelector("#cancelButton"),
  peerName: document.querySelector("#peerName"),
  peerAvatar: document.querySelector("#peerAvatar"),
  connectionLabel: document.querySelector("#connectionLabel"),
  chatCode: document.querySelector("#chatCode"),
  messages: document.querySelector("#messages"),
  systemIntro: document.querySelector("#systemIntro"),
  transportLabel: document.querySelector("#transportLabel"),
  form: document.querySelector("#messageForm"),
  messageInput: document.querySelector("#messageInput"),
  send: document.querySelector("#sendButton"),
  leave: document.querySelector("#leaveButton")
};

const state = {
  name: "",
  roomCode: "",
  peerId: "",
  otherPeerId: "",
  lastEventId: 0,
  polling: false,
  rtc: null,
  channel: null,
  transport: "",
  pendingCandidates: [],
  leaving: false
};

// transport=relay is a small diagnostics switch used to verify the fallback.
const forceRelay = new URLSearchParams(location.search).get("transport") === "relay";
const supportsWebRTC = !forceRelay && typeof window.RTCPeerConnection === "function";

const roomFromURL = new URLSearchParams(location.search).get("room");
if (roomFromURL) elements.codeInput.value = normalizeCode(roomFromURL);
elements.name.value = localStorage.getItem("localchat-name") || "";

elements.create.addEventListener("click", createRoom);
elements.join.addEventListener("click", joinRoom);
elements.copyCode.addEventListener("click", () => copyText(state.roomCode, elements.copyCode.querySelector("small")));
elements.copyLink.addEventListener("click", () => copyText(inviteURL(), elements.copyLink));
elements.cancel.addEventListener("click", leaveRoom);
elements.leave.addEventListener("click", leaveRoom);
elements.form.addEventListener("submit", sendMessage);
elements.codeInput.addEventListener("input", () => {
  elements.codeInput.value = normalizeCode(elements.codeInput.value);
});
elements.messageInput.addEventListener("input", resizeComposer);
elements.messageInput.addEventListener("keydown", event => {
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    elements.form.requestSubmit();
  }
});

window.addEventListener("beforeunload", notifyLeave);

async function createRoom() {
  if (!captureName()) return;
  setBusy(true);
  try {
    const room = await api(`/api/rooms?rtc=${supportsWebRTC ? "1" : "0"}`, { method: "POST" });
    state.roomCode = room.code;
    state.peerId = room.peerId;
    updateURL();
    elements.waitingCode.textContent = room.code;
    showView("waiting");
    startPolling();
  } catch (error) {
    showError(error.message);
  } finally {
    setBusy(false);
  }
}

async function joinRoom() {
  if (!captureName()) return;
  const code = normalizeCode(elements.codeInput.value);
  if (code.length !== 6) {
    showError("Enter the 6-character room code.");
    elements.codeInput.focus();
    return;
  }
  setBusy(true);
  try {
    const room = await api(`/api/rooms/${encodeURIComponent(code)}/join?rtc=${supportsWebRTC ? "1" : "0"}`, { method: "POST" });
    state.roomCode = room.code;
    state.peerId = room.peerId;
    state.otherPeerId = room.otherPeerId;
    updateURL();
    elements.waitingCode.textContent = room.code;
    elements.waitingStatus.innerHTML = "<i></i> Connecting to the other person…";
    showView("waiting");
    startPolling();
    if (!supportsWebRTC || !room.otherSupportsWebRTC) await activateRelay();
  } catch (error) {
    showError(error.message);
  } finally {
    setBusy(false);
  }
}

function captureName() {
  const name = elements.name.value.trim();
  if (!name) {
    showError("Enter your name first.");
    elements.name.focus();
    return false;
  }
  state.name = name.slice(0, 28);
  localStorage.setItem("localchat-name", state.name);
  showError("");
  return true;
}

async function startPolling() {
  if (state.polling) return;
  state.polling = true;
  while (state.polling && !state.leaving) {
    try {
      const query = new URLSearchParams({ peer: state.peerId, since: String(state.lastEventId) });
      const data = await api(`/api/rooms/${state.roomCode}/events?${query}`);
      for (const event of data.events) {
        state.lastEventId = Math.max(state.lastEventId, event.id);
        await handleSignal(event);
      }
    } catch (error) {
      if (!state.polling || state.leaving) break;
      setConnectionState(`Setup interrupted — retrying…`, false);
      await delay(1200);
    }
  }
}

async function handleSignal(event) {
  if (event.type === "peer-joined") {
    state.otherPeerId = event.from;
    if (supportsWebRTC && event.payload?.supportsWebRTC) {
      elements.waitingStatus.innerHTML = "<i></i> Person found — creating a direct connection…";
      await createPeerConnection(true);
    } else {
      elements.waitingStatus.innerHTML = "<i></i> Person found — opening the local chat…";
      await activateRelay();
    }
    return;
  }
  if (event.type === "peer-left") {
    peerDisconnected("The other person left the room.");
    return;
  }

  if (event.type === "relay-hello") {
    if (state.transport !== "relay") await activateRelay();
    setPeerName(event.payload?.name);
    return;
  }
  if (event.type === "relay-chat") {
    if (state.transport !== "relay") await activateRelay();
    if (typeof event.payload?.text === "string") {
      renderMessage(event.payload.text.slice(0, 2000), false, event.payload.sentAt);
    }
    return;
  }
  if (!state.rtc) await createPeerConnection(false);

  if (event.type === "offer") {
    await state.rtc.setRemoteDescription(event.payload);
    await addPendingCandidates();
    const answer = await state.rtc.createAnswer();
    await state.rtc.setLocalDescription(answer);
    await sendSignal("answer", state.rtc.localDescription);
  } else if (event.type === "answer") {
    await state.rtc.setRemoteDescription(event.payload);
    await addPendingCandidates();
  } else if (event.type === "candidate") {
    if (state.rtc.remoteDescription) {
      await state.rtc.addIceCandidate(event.payload);
    } else {
      state.pendingCandidates.push(event.payload);
    }
  }
}

async function createPeerConnection(initiator) {
  if (state.rtc) return;
  if (!supportsWebRTC) {
    await activateRelay();
    return;
  }
  // No public STUN/TURN server is used. Peers discover one another using local
  // network ICE candidates exchanged through the small Go signaling service.
  state.rtc = new RTCPeerConnection({ iceServers: [] });

  state.rtc.addEventListener("icecandidate", event => {
    if (event.candidate) sendSignal("candidate", event.candidate.toJSON()).catch(connectionError);
  });
  state.rtc.addEventListener("connectionstatechange", () => {
    const status = state.rtc?.connectionState;
    if (status === "failed") {
      activateRelay().catch(connectionError);
    } else if (status === "disconnected") {
      setConnectionState("Direct connection interrupted…", false);
    }
  });
  state.rtc.addEventListener("datachannel", event => configureChannel(event.channel));

  if (initiator) {
    configureChannel(state.rtc.createDataChannel("localchat", { ordered: true }));
    const offer = await state.rtc.createOffer();
    await state.rtc.setLocalDescription(offer);
    await sendSignal("offer", state.rtc.localDescription);
  }
}

function configureChannel(channel) {
  state.channel = channel;
  channel.addEventListener("open", () => {
    state.transport = "webrtc";
    openChat("Direct WebRTC", "You’re connected. Messages now travel directly between both devices.");
    channel.send(JSON.stringify({ type: "hello", name: state.name }));
    elements.messageInput.focus();
  });
  channel.addEventListener("message", event => {
    try {
      const message = JSON.parse(event.data);
      if (message.type === "hello") {
        setPeerName(message.name);
      } else if (message.type === "chat" && typeof message.text === "string") {
        renderMessage(message.text.slice(0, 2000), false, message.sentAt);
      }
    } catch {
      // Ignore data that does not match the tiny chat protocol.
    }
  });
  channel.addEventListener("close", () => {
    if (state.transport === "webrtc") peerDisconnected("The other person left the chat.");
  });
  channel.addEventListener("error", () => {
    if (state.transport === "webrtc") activateRelay().catch(connectionError);
  });
}

async function activateRelay() {
  if (state.transport === "relay") return;
  state.transport = "relay";
  if (state.channel) state.channel.close();
  if (state.rtc) {
    state.rtc.close();
    state.rtc = null;
  }
  openChat("Local Go relay", "You’re connected. Messages stay on this Wi-Fi and pass through the local Go app.");
  elements.messageInput.focus();
  await sendSignal("relay-hello", { name: state.name });
}

function openChat(transportLabel, intro) {
  showView("chat");
  elements.chatCode.textContent = state.roomCode;
  elements.transportLabel.textContent = transportLabel;
  elements.systemIntro.textContent = intro;
  elements.connectionLabel.innerHTML = "<i></i> Connected";
  elements.messageInput.disabled = false;
  elements.send.disabled = false;
}

function setPeerName(name) {
  const safeName = String(name || "Nearby person").slice(0, 28);
  elements.peerName.textContent = safeName;
  elements.peerAvatar.textContent = initial(safeName);
}

async function addPendingCandidates() {
  const queued = state.pendingCandidates.splice(0);
  for (const candidate of queued) await state.rtc.addIceCandidate(candidate);
}

async function sendSignal(type, payload) {
  const query = new URLSearchParams({ peer: state.peerId });
  await api(`/api/rooms/${state.roomCode}/signal?${query}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ to: state.otherPeerId, type, payload })
  });
}

async function sendMessage(event) {
  event.preventDefault();
  const text = elements.messageInput.value.trim();
  if (!text) return;
  const sentAt = new Date().toISOString();
  elements.send.disabled = true;
  try {
    if (state.transport === "webrtc" && state.channel?.readyState === "open") {
      state.channel.send(JSON.stringify({ type: "chat", text, sentAt }));
    } else if (state.transport === "relay") {
      await sendSignal("relay-chat", { text, sentAt });
    } else {
      return;
    }
    renderMessage(text, true, sentAt);
    elements.messageInput.value = "";
    resizeComposer();
  } catch (error) {
    appendSystemMessage(error.message || "The message could not be sent.");
  } finally {
    elements.send.disabled = false;
  }
}

function renderMessage(text, mine, sentAt) {
  const row = document.createElement("div");
  row.className = `message-row${mine ? " mine" : ""}`;
  const bubble = document.createElement("div");
  bubble.className = "bubble";
  bubble.textContent = text;
  const meta = document.createElement("div");
  meta.className = "message-meta";
  meta.textContent = `${mine ? "You" : elements.peerName.textContent} · ${formatTime(sentAt)}`;
  row.append(bubble, meta);
  elements.messages.append(row);
  elements.messages.scrollTop = elements.messages.scrollHeight;
}

function peerDisconnected(message) {
  if (state.leaving) return;
  setConnectionState(message, false);
  elements.messageInput.disabled = true;
  elements.send.disabled = true;
  appendSystemMessage(message);
}

function appendSystemMessage(text) {
  if (!elements.chat.classList.contains("hidden")) {
    const notice = document.createElement("div");
    notice.className = "system-message";
    notice.textContent = text;
    elements.messages.append(notice);
    elements.messages.scrollTop = elements.messages.scrollHeight;
  }
}

function setConnectionState(text, connected) {
  elements.connectionLabel.innerHTML = `<i></i> ${escapeText(text)}`;
  const dot = elements.connectionLabel.querySelector("i");
  if (!connected) dot.style.background = "#ffaaa4";
}

function leaveRoom() {
  state.leaving = true;
  notifyLeave();
  state.polling = false;
  if (state.channel) state.channel.close();
  if (state.rtc) state.rtc.close();
  location.href = location.pathname;
}

function notifyLeave() {
  if (!state.roomCode || !state.peerId) return;
  const query = new URLSearchParams({ peer: state.peerId });
  navigator.sendBeacon(`/api/rooms/${state.roomCode}/leave?${query}`, new Blob([], { type: "text/plain" }));
}

function showView(view) {
  elements.welcome.classList.toggle("hidden", view !== "welcome");
  elements.waiting.classList.toggle("hidden", view !== "waiting");
  elements.chat.classList.toggle("hidden", view !== "chat");
}

function setBusy(busy) {
  elements.create.disabled = busy;
  elements.join.disabled = busy;
}

function showError(message) { elements.error.textContent = message; }
function normalizeCode(value) { return String(value || "").toUpperCase().replace(/[^A-Z2-9]/g, "").slice(0, 6); }
function inviteURL() { return `${location.origin}${location.pathname}?room=${encodeURIComponent(state.roomCode)}`; }
function updateURL() { history.replaceState({}, "", `${location.pathname}?room=${encodeURIComponent(state.roomCode)}`); }
function initial(name) { return Array.from(name.trim())[0]?.toUpperCase() || "?"; }
function delay(ms) { return new Promise(resolve => setTimeout(resolve, ms)); }
function formatTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "now" : date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
function resizeComposer() {
  elements.messageInput.style.height = "auto";
  elements.messageInput.style.height = `${Math.min(elements.messageInput.scrollHeight, 120)}px`;
}
function connectionError(error) { peerDisconnected(error?.message || "Connection setup failed."); }
function escapeText(value) {
  const span = document.createElement("span");
  span.textContent = value;
  return span.innerHTML;
}

async function copyText(text, feedbackElement) {
  try {
    await navigator.clipboard.writeText(text);
    const previous = feedbackElement.textContent;
    feedbackElement.textContent = "Copied!";
    setTimeout(() => { feedbackElement.textContent = previous; }, 1200);
  } catch {
    window.prompt("Copy this:", text);
  }
}

async function api(path, options = {}) {
  const response = await fetch(path, options);
  if (response.status === 204) return null;
  let data;
  try { data = await response.json(); } catch { data = {}; }
  if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
  return data;
}
