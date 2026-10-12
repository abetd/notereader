"use strict";

const $ = (id) => document.getElementById(id);
const q = $("q");
const sort = $("sort");
const results = $("results");
const searchError = $("search-error");
const statusEl = $("status");
const preview = $("preview");
const scopeSel = $("scope");
const archiveBox = $("archive");
// 絞り込み中のルート名（/{name} で開いたとき）。空なら全ルートが対象
const scope = document.body.dataset.scope || "";

// URL のクエリ文字列から検索状態を復元する（ブックマーク・戻る操作用）
const params = new URLSearchParams(location.search);
q.value = params.get("q") ?? "";
sort.value = params.get("sort") === "score" ? "score" : "mtime";
// アーカイブは既定では一覧に出さない（?archive=1 で含める）
archiveBox.checked = params.get("archive") === "1";
if (document.body.dataset.scope) document.title = `notereader — ${document.body.dataset.scope}`;

let timer;
let seq = 0;
let hits = [];
let sel = -1; // 選択中の結果のインデックス（-1 は未選択）
let lastSearch = Promise.resolve();
let previewTimer;
let lastSync;
// 直近の /api/status の応答（チェックの切り替えで件数を描き直すのに使う）
let lastStatus;
// URL の id。リロードしても同じファイルを選び直すために使う
let restoreId = params.get("id") ?? undefined;
// プレビューに表示中のファイル（中身が変わったら読み込み直す）
let shown = { id: undefined, mtime: undefined };

// keepSelection: 自動更新など、選択中のファイルを保ったまま取り直したいとき
function runSearch(keepSelection = false) {
  clearTimeout(timer);
  timer = undefined;
  lastSearch = doSearch(keepSelection);
  return lastSearch;
}

// fetchJSON は JSON の API を呼ぶ。通信の失敗・2xx 以外・JSON でない応答（Host 検査の 403 など）は、
// 理由を message に、HTTP のステータスを status に持つ Error にする
async function fetchJSON(url, opts) {
  let res;
  try {
    res = await fetch(url, opts);
  } catch {
    throw Object.assign(new Error("サーバーに接続できません"), { status: 0 });
  }
  let data;
  try {
    data = await res.json();
  } catch {
    throw Object.assign(new Error(`応答を読めません（HTTP ${res.status}）`), { status: res.status });
  }
  if (!res.ok) {
    throw Object.assign(new Error(data.error ?? `HTTP ${res.status}`), { status: res.status });
  }
  return data;
}

// showNotice は右上のお知らせを出す。text が空なら隠す
function showNotice(el, text, { error = false, title = "" } = {}) {
  el.hidden = text === "";
  el.textContent = text;
  el.title = title || text;
  el.classList.toggle("error", error);
}

// 失敗しても例外を外へ出さない（Enter の await lastSearch が抜けて開けなくなるため）
async function doSearch(keepSelection) {
  const my = ++seq;
  const p = new URLSearchParams({ q: q.value, sort: sort.value, limit: "50" });
  if (scope) p.set("root", scope);
  if (archiveBox.checked) p.set("archive", "1");
  syncURL();
  let data;
  try {
    data = await fetchJSON(`/api/search?${p}`);
  } catch (err) {
    if (my === seq) showNotice(searchError, `検索に失敗しました: ${err.message}`, { error: true });
    return;
  }
  if (my !== seq) return; // 古い応答は捨てる
  showNotice(searchError, "");
  // 選択の引き継ぎは応答を反映する直前に決める。
  // 通信中にユーザーが選び直していた場合、その選択を上書きしないため
  const wantId = keepSelection ? hits[sel]?.id : restoreId;
  restoreId = undefined;
  const prevSel = sel;
  hits = data.hits;
  results.replaceChildren(...hits.map(renderHit));
  $("empty").hidden = hits.length > 0;
  let initial = 0;
  if (wantId !== undefined) {
    const found = hits.findIndex((h) => h.id === wantId);
    if (found >= 0) initial = found;
  }
  // 位置が変わらない自動更新では、読んでいる途中のスクロールを動かさない
  const sameSpot = keepSelection && initial === prevSel;
  sel = -1; // 作り直した DOM に対して選択し直す
  select(hits.length > 0 ? initial : -1, !sameSpot);
}

function basePath() {
  return scope ? `/${encodeURIComponent(scope)}` : "/";
}

// キー移動での URL 更新は間引く（履歴 API には短時間の呼び出し回数制限があるため）
let urlTimer;
function scheduleURLSync() {
  clearTimeout(urlTimer);
  urlTimer = setTimeout(syncURL, 150);
}

// 検索語・並び順・アーカイブを含めるか・選択中のファイルを URL に反映する（リロードや共有で同じ状態に戻せる）
function syncURL() {
  clearTimeout(urlTimer);
  const p = new URLSearchParams();
  if (q.value) p.set("q", q.value);
  if (sort.value !== "mtime") p.set("sort", sort.value);
  if (archiveBox.checked) p.set("archive", "1");
  const id = hits[sel]?.id;
  if (id) p.set("id", id);
  const qs = p.toString();
  history.replaceState(history.state, "", qs ? `${basePath()}?${qs}` : basePath());
}

function renderHit(h, i) {
  const li = document.createElement("li");
  li.id = `hit-${i}`;
  li.setAttribute("role", "option");
  // クリックで選択（プレビューに出す）。開くのは ⌘/Ctrl+Enter か「新しいタブ」
  li.addEventListener("click", () => select(i));

  const head = document.createElement("div");
  head.className = "hit-head";
  const badge = document.createElement("span");
  badge.className = "badge";
  badge.textContent = h.root;
  const kind = document.createElement("span");
  kind.className = `badge kind kind-${h.type}`;
  kind.textContent = kindLabel(h.type);
  const when = document.createElement("span");
  when.className = "when";
  const t = new Date(h.mtime);
  when.textContent = relativeTime(t);
  when.title = t.toLocaleString("ja-JP");
  const badges = document.createElement("span");
  badges.className = "hit-badges";
  badges.append(badge, kind);
  if (h.archived) {
    const arc = document.createElement("span");
    arc.className = "badge archived";
    arc.textContent = "アーカイブ";
    badges.append(arc);
  }
  head.append(badges, when);
  const title = document.createElement("div");
  title.className = "title";
  title.textContent = h.title;
  const snip = document.createElement("div");
  snip.className = "snippet";
  // snippet はサーバー側でエスケープ済み（<mark> のみ含む）
  snip.innerHTML = h.snippet;

  li.append(head, title, snip);
  return li;
}

const kindLabels = { html: "HTML", markdown: "MD" };
function kindLabel(type) {
  return kindLabels[type] ?? type;
}

// urlNow: URL をすぐ書き換えるか（キー移動だけは間引く）
function select(i, scroll = true, urlNow = true) {
  results.children[sel]?.classList.remove("sel");
  results.children[sel]?.setAttribute("aria-selected", "false");
  sel = i;
  const li = results.children[sel];
  if (!li) {
    q.removeAttribute("aria-activedescendant");
    showPreview(null);
    return;
  }
  li.classList.add("sel");
  li.setAttribute("aria-selected", "true");
  q.setAttribute("aria-activedescendant", li.id);
  if (scroll) li.scrollIntoView({ block: "nearest" });
  if (urlNow) syncURL();
  else scheduleURLSync();
  showPreview(hits[sel]);
}

// プレビューの読み込みは少し待つ（キーを押しっぱなしで移動している間は読み込まない）
function showPreview(h) {
  clearTimeout(previewTimer);
  const head = $("preview-head");
  if (!h) {
    head.hidden = true;
    preview.hidden = true;
    preview.removeAttribute("src");
    $("preview-empty").hidden = false;
    return;
  }
  head.hidden = false;
  $("preview-empty").hidden = true;
  $("preview-badge").textContent = h.root;
  const pk = $("preview-kind");
  pk.textContent = kindLabel(h.type);
  pk.className = `badge kind kind-${h.type}`;
  $("preview-archive").hidden = !h.archived;
  $("preview-name").textContent = h.title;
  $("preview-path").textContent = h.rel_path;
  const t = new Date(h.mtime);
  $("preview-time").textContent = relativeTime(t);
  $("preview-time").title = t.toLocaleString("ja-JP");
  previewTimer = setTimeout(() => {
    preview.hidden = false;
    if (shown.id !== h.id) {
      preview.setAttribute("src", h.url);
    } else if (shown.mtime !== h.mtime) {
      // 同じファイルの中身が変わったとき。src は同じなので明示的に読み込み直す
      try {
        preview.contentWindow.location.reload();
      } catch {
        preview.setAttribute("src", h.url);
      }
    }
    shown = { id: h.id, mtime: h.mtime };
  }, 150);
}

// 今日は時刻、昨日は「昨日」、10 日前までは「n 日前」、それより前は日付。
// 経過時間ではなく暦日で数える（23:50 の更新を翌 0:10 に見たら「昨日」）
function relativeTime(d) {
  const startOfDay = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const days = Math.round((startOfDay(new Date()) - startOfDay(d)) / 86400000);
  if (days <= 0) return d.toLocaleTimeString("ja-JP", { hour: "2-digit", minute: "2-digit" });
  if (days === 1) return "昨日";
  if (days <= 10) return `${days} 日前`;
  return d.toLocaleDateString("ja-JP");
}

function move(delta) {
  if (hits.length === 0) return;
  select(Math.min(Math.max(sel + delta, 0), hits.length - 1), true, false);
}

// 新しいタブで開く（この画面は検索のために残す）
function openHit(i) {
  const h = hits[i];
  if (h) window.open(h.url, "_blank", "noopener");
}

document.addEventListener("keydown", async (e) => {
  // IME 変換中のキー（変換確定の Enter など）は触らない
  if (e.isComposing || e.keyCode === 229) return;
  const inInput = e.target === q;
  const onControl = !inInput && e.target.closest?.("input, select, textarea, button, summary");

  if (e.key === "/" && !inInput && !onControl) {
    e.preventDefault();
    q.focus();
    q.select();
    return;
  }
  // Esc はヘルプやボタンにフォーカスがあるときも効かせる
  if (e.key === "Escape") {
    if ($("help").open) {
      $("help").open = false;
    } else if (q.value !== "") {
      q.value = "";
      runSearch();
    }
    q.focus();
    return;
  }
  if (onControl) return;

  const ctrlOnly = e.ctrlKey && !e.metaKey && !e.altKey;
  if (e.key === "ArrowDown" || (ctrlOnly && e.key === "n")) {
    e.preventDefault();
    move(1);
  } else if (e.key === "ArrowUp" || (ctrlOnly && e.key === "p")) {
    e.preventDefault();
    move(-1);
  } else if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
    e.preventDefault();
    // 入力直後の Enter は、最新の検索結果が出てから開く
    if (timer !== undefined) await runSearch();
    else await lastSearch;
    openHit(sel);
  }
});

// ヘルプは外をクリックしたら閉じる
document.addEventListener("click", (e) => {
  if ($("help").open && !$("help").contains(e.target)) $("help").open = false;
});

// 状態を取り直して画面に反映する。取れなければ右上に理由を出し、undefined を返す
async function refreshStatus() {
  let s;
  try {
    s = await fetchJSON("/api/status");
  } catch (err) {
    showNotice(statusEl, err.status === 0 ? err.message : `状態を取得できません: ${err.message}`, { error: true });
    return undefined;
  }
  lastStatus = s;
  if (lastSync === undefined) lastSync = s.sync.last_sync;
  renderCounts(s);
  return s;
}

// 件数は一覧に合わせる（アーカイブを含めないときは、アーカイブ内の件数を引く）
function visibleCount(x) {
  return archiveBox.checked ? x.documents : x.documents - x.archived;
}

function renderCounts(s) {
  renderScopes(s.roots);
  renderSync(s.sync);
}

// 同期中と、直近の同期の失敗だけを出す（うまくいっているときは何も出さない）
function renderSync(sync) {
  if (sync.syncing) {
    showNotice(statusEl, "同期中…");
  } else if (sync.error) {
    showNotice(statusEl, `同期エラー: ${sync.error}`, { error: true });
  } else if (sync.last_result.failed > 0) {
    showNotice(statusEl, `読めないファイル ${sync.last_result.failed} 件`, {
      error: true,
      title: `直近の同期で ${sync.last_result.failed} 件のファイルを読めませんでした（理由はログにあります）`,
    });
  } else {
    showNotice(statusEl, "");
  }
}

// ルートの切り替えは画面遷移（URL を /{name} にするため）。選択肢は最初に作り、件数は毎回書き直す
function renderScopes(roots) {
  if (!scopeSel.dataset.ready) {
    for (const r of roots) {
      const opt = document.createElement("option");
      opt.value = r.name;
      scopeSel.append(opt);
    }
    scopeSel.value = scope;
    scopeSel.dataset.ready = "1";
  }
  for (const r of roots) {
    const opt = [...scopeSel.options].find((o) => o.value === r.name);
    if (opt) opt.textContent = `${r.name}（${visibleCount(r)}）`;
  }
}

// 検索語・並び順・アーカイブを含めるかは、ルートを切り替えても保つ
scopeSel.addEventListener("change", () => {
  const base = scopeSel.value ? `/${encodeURIComponent(scopeSel.value)}` : "/";
  const p = new URLSearchParams();
  if (q.value) p.set("q", q.value);
  if (sort.value !== "mtime") p.set("sort", sort.value);
  if (archiveBox.checked) p.set("archive", "1");
  const qs = p.toString();
  location.href = qs ? `${base}?${qs}` : base;
});

// 選択中のファイルが切り替え後の結果にもあれば、選んだままにする
archiveBox.addEventListener("change", () => {
  if (lastStatus) renderCounts(lastStatus);
  runSearch(true);
});

q.addEventListener("input", () => {
  clearTimeout(timer);
  timer = setTimeout(runSearch, 200);
});
sort.addEventListener("change", runSearch);
$("open-tab").addEventListener("click", () => openHit(sel));

// インデックスが更新されたら検索結果を取り直す（ファイル監視での追加・削除に追従する）
async function pollIndex() {
  const s = await refreshStatus();
  if (!s || s.sync.last_sync === lastSync) return;
  lastSync = s.sync.last_sync;
  await runSearch(true);
}

runSearch();
refreshStatus();
setInterval(pollIndex, 3000);
