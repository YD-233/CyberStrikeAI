/**
 * 黑板实时面板（Cairn OODA 编排）。
 *
 * 在对话页以抽屉形式展示一次黑板运行的实时状态：
 *   - OODA 阶段（Bootstrap → Reason → Explore → Synthesize）+ 轮次
 *   - Fact（确认发现，复用 project_facts）/ Intent（待探索方向，按状态分组）/ Hint（人类注入）
 * 数据来源：运行期由 monitor.js 转发的 SSE 黑板事件增量更新；
 *           打开时从 REST（/facts /intents /hints）拉一次快照对齐历史。
 * 协调语义：worker 之间只通过黑板间接协调（Stigmergy），本面板即黑板的人类视图。
 */
(function (global) {
    'use strict';

    // ---- 运行态 ----
    let _open = false;
    let _projectId = '';
    let _conversationId = '';
    let _phase = '';
    let _round = 0;
    let _active = false;        // 是否有黑板运行正在进行（收到过事件）
    let _seededFor = '';        // 已对该 projectId 做过 REST 播种
    let _renderQueued = false;

    const _facts = new Map();   // factKey -> {factKey, category, confidence, summary, pinned}
    const _intents = new Map(); // intentId -> {id, title, body, priority, status, claimedBy, resultFactKey, resultSummary}
    const _hints = new Map();   // hintId -> {id, content}

    const PLACEHOLDER_PREFIX = '__bbph__'; // 事件先于 REST 到达时，用 title 占位的合成 key

    function tt(key, fallback, opts) {
        if (typeof global.t === 'function') {
            try {
                const v = global.t(key, opts);
                if (v && v !== key) return v;
            } catch (e) { /* ignore */ }
        }
        return fallback;
    }

    function esc(s) {
        return String(s == null ? '' : s)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    function el(id) { return document.getElementById(id); }

    // ---- 阶段标签 ----
    function phaseLabel(phase) {
        switch (String(phase || '').toLowerCase()) {
            case 'bootstrap': return tt('chat.bbPhaseBootstrap', '引导');
            case 'reason': return tt('chat.bbPhaseReason', '推理');
            case 'explore': return tt('chat.bbPhaseExplore', '探索');
            case 'synthesize': return tt('chat.bbPhaseSynthesize', '综合');
            default: return phase || '';
        }
    }

    function phaseGlyph(phase) {
        switch (String(phase || '').toLowerCase()) {
            case 'bootstrap': return '🚀';
            case 'reason': return '🧠';
            case 'explore': return '🔍';
            case 'synthesize': return '📋';
            default: return '◆';
        }
    }

    // ---- 渲染（批处理，避免高频事件抖动） ----
    function scheduleRender() {
        if (_renderQueued) return;
        _renderQueued = true;
        (global.requestAnimationFrame || function (cb) { setTimeout(cb, 16); })(function () {
            _renderQueued = false;
            render();
        });
    }
    function render() {
        const root = el('blackboard-panel');
        if (!root) return;

        // 阶段徽标
        const phaseEl = el('bb-phase-badge');
        if (phaseEl) {
            if (_phase) {
                const roundTxt = _round > 0
                    ? ' · ' + tt('chat.bbRound', '第 {n} 轮', { n: _round }).replace('{n}', _round)
                    : '';
                phaseEl.textContent = phaseGlyph(_phase) + ' ' + phaseLabel(_phase) + roundTxt;
                phaseEl.classList.remove('bb-phase-idle');
                phaseEl.setAttribute('data-phase', String(_phase).toLowerCase());
            } else {
                phaseEl.textContent = tt('chat.bbPhaseIdle', '未运行');
                phaseEl.classList.add('bb-phase-idle');
                phaseEl.removeAttribute('data-phase');
            }
        }

        renderFacts();
        renderIntents();
        renderHints();
        updateCounts();
    }

    function setCount(id, n) {
        const c = el(id);
        if (c) c.textContent = String(n);
    }

    function updateCounts() {
        setCount('bb-count-facts', _facts.size);
        setCount('bb-count-intents', _intents.size);
        setCount('bb-count-hints', _hints.size);
    }

    function confidenceClass(conf) {
        const c = String(conf || '').toLowerCase();
        if (c === 'confirmed') return 'bb-conf-confirmed';
        if (c === 'tentative') return 'bb-conf-tentative';
        if (c === 'deprecated') return 'bb-conf-deprecated';
        return 'bb-conf-default';
    }

    function renderFacts() {
        const wrap = el('bb-facts-list');
        if (!wrap) return;
        if (_facts.size === 0) {
            wrap.innerHTML = '<div class="bb-empty">' + esc(tt('chat.bbFactsEmpty', '暂无事实')) + '</div>';
            return;
        }
        const items = Array.from(_facts.values());
        // pinned 优先，其余按 key 稳定排序
        items.sort(function (a, b) {
            if (!!b.pinned !== !!a.pinned) return b.pinned ? 1 : -1;
            return String(a.factKey).localeCompare(String(b.factKey));
        });
        wrap.innerHTML = items.map(function (f) {
            const conf = f.confidence ? '<span class="bb-fact-conf ' + confidenceClass(f.confidence) + '">' + esc(f.confidence) + '</span>' : '';
            const cat = f.category ? '<span class="bb-fact-cat">' + esc(f.category) + '</span>' : '';
            const pin = f.pinned ? '<span class="bb-fact-pin" title="pinned">📌</span>' : '';
            const summary = f.summary ? '<div class="bb-card-summary">' + esc(f.summary) + '</div>' : '';
            return '<div class="bb-card bb-fact-card">'
                + '<div class="bb-card-head">' + pin + '<code class="bb-fact-key">' + esc(f.factKey) + '</code>' + cat + conf + '</div>'
                + summary
                + '</div>';
        }).join('');
    }
    function intentStatusMeta(status) {
        switch (String(status || '').toLowerCase()) {
            case 'open': return { cls: 'bb-int-open', label: tt('chat.bbIntentOpen', '待探索'), glyph: '○' };
            case 'claimed': return { cls: 'bb-int-claimed', label: tt('chat.bbIntentClaimed', '执行中'), glyph: '◐' };
            case 'done': return { cls: 'bb-int-done', label: tt('chat.bbIntentDone', '已完成'), glyph: '●' };
            case 'dropped': return { cls: 'bb-int-dropped', label: tt('chat.bbIntentDropped', '已放弃'), glyph: '✕' };
            default: return { cls: 'bb-int-open', label: status || '', glyph: '○' };
        }
    }

    function renderIntents() {
        const wrap = el('bb-intents-list');
        if (!wrap) return;
        if (_intents.size === 0) {
            wrap.innerHTML = '<div class="bb-empty">' + esc(tt('chat.bbIntentsEmpty', '暂无意图')) + '</div>';
            return;
        }
        // 分组顺序：执行中 → 待探索 → 已完成 → 已放弃
        const order = { claimed: 0, open: 1, done: 2, dropped: 3 };
        const items = Array.from(_intents.values());
        items.sort(function (a, b) {
            const oa = order[String(a.status).toLowerCase()] != null ? order[String(a.status).toLowerCase()] : 9;
            const ob = order[String(b.status).toLowerCase()] != null ? order[String(b.status).toLowerCase()] : 9;
            if (oa !== ob) return oa - ob;
            return (b.priority || 0) - (a.priority || 0);
        });
        wrap.innerHTML = items.map(function (it) {
            const meta = intentStatusMeta(it.status);
            const prio = it.priority ? '<span class="bb-int-prio" title="priority">P' + esc(it.priority) + '</span>' : '';
            const claimed = it.claimedBy ? '<span class="bb-int-worker" title="claimed by">' + esc(it.claimedBy) + '</span>' : '';
            const body = it.body ? '<div class="bb-card-summary">' + esc(it.body) + '</div>' : '';
            let result = '';
            if (String(it.status).toLowerCase() === 'done' && (it.resultFactKey || it.resultSummary)) {
                const rk = it.resultFactKey ? '<code class="bb-int-result-key">→ ' + esc(it.resultFactKey) + '</code>' : '';
                const rs = it.resultSummary ? '<span class="bb-int-result-sum">' + esc(it.resultSummary) + '</span>' : '';
                result = '<div class="bb-int-result">' + rk + rs + '</div>';
            }
            return '<div class="bb-card bb-int-card ' + meta.cls + '">'
                + '<div class="bb-card-head">'
                + '<span class="bb-int-status" title="' + esc(meta.label) + '">' + meta.glyph + '</span>'
                + '<span class="bb-int-title">' + esc(it.title) + '</span>'
                + prio + claimed
                + '</div>'
                + body + result
                + '</div>';
        }).join('');
    }

    function renderHints() {
        const wrap = el('bb-hints-list');
        if (!wrap) return;
        if (_hints.size === 0) {
            wrap.innerHTML = '<div class="bb-empty">' + esc(tt('chat.bbHintsEmpty', '暂无人类提示')) + '</div>';
            return;
        }
        wrap.innerHTML = Array.from(_hints.values()).map(function (h) {
            return '<div class="bb-card bb-hint-card">'
                + '<span class="bb-hint-glyph" aria-hidden="true">💡</span>'
                + '<div class="bb-card-summary">' + esc(h.content) + '</div>'
                + '</div>';
        }).join('');
    }
    // ---- 事件吸收 ----
    // 事件可能先于 REST 快照到达；intentId 缺失时用 title 合成占位 key，REST 播种时再对齐。
    function upsertIntentFromEvent(data, status) {
        const id = data.intentId || (PLACEHOLDER_PREFIX + String(data.title || '').trim());
        const prev = _intents.get(id) || {};
        const it = {
            id: data.intentId || prev.id || id,
            title: data.title != null ? data.title : prev.title,
            body: data.body != null && data.body !== '' ? data.body : prev.body,
            priority: data.priority != null ? data.priority : prev.priority,
            status: status || prev.status || 'open',
            claimedBy: data.claimedBy != null && data.claimedBy !== '' ? data.claimedBy : prev.claimedBy,
            resultFactKey: data.resultFactKey != null ? data.resultFactKey : prev.resultFactKey,
            resultSummary: data.resultSummary != null ? data.resultSummary : prev.resultSummary,
        };
        _intents.set(id, it);
    }

    // handleBoardEvent 由 monitor.js 在 SSE 事件分发时调用。
    function handleBoardEvent(type, data) {
        data = data || {};
        _active = true;
        if (data.conversationId) _conversationId = data.conversationId;
        if (data.projectId && data.projectId !== _projectId) {
            _projectId = data.projectId;
            // 已知 projectId：异步播种历史快照（仅一次）。
            seedFromProject(_projectId);
        }
        switch (type) {
            case 'ooda_phase':
                _phase = data.phase || _phase;
                if (data.round != null) _round = data.round;
                break;
            case 'fact_added':
                if (data.factKey) {
                    _facts.set(data.factKey, {
                        factKey: data.factKey,
                        category: data.category || '',
                        confidence: data.confidence || '',
                        summary: data.factSummary || data.summary || '',
                        pinned: !!data.pinned,
                    });
                }
                break;
            case 'intent_added':
                upsertIntentFromEvent(data, 'open');
                break;
            case 'intent_claimed':
                upsertIntentFromEvent(data, 'claimed');
                break;
            case 'intent_done':
                upsertIntentFromEvent(data, 'done');
                break;
            case 'intent_dropped':
                upsertIntentFromEvent(data, 'dropped');
                break;
            case 'hint_added':
                if (data.hintId || data.content) {
                    const hid = data.hintId || ('h' + Date.now());
                    _hints.set(hid, { id: hid, content: data.content || data.summary || '' });
                }
                break;
            default:
                return; // 非黑板事件
        }
        updateToggleActivity();
        scheduleRender();
    }
    // ---- REST 播种：打开面板或首次得知 projectId 时拉一次历史快照 ----
    function mergeFact(f) {
        if (!f || !f.fact_key) return;
        // 事件已写入的以事件为准（更实时），仅补全缺失字段。
        const prev = _facts.get(f.fact_key) || {};
        _facts.set(f.fact_key, {
            factKey: f.fact_key,
            category: prev.category || f.category || '',
            confidence: prev.confidence || f.confidence || '',
            summary: prev.summary || f.summary || '',
            pinned: prev.pinned || !!f.pinned,
        });
    }

    function mergeIntent(it) {
        if (!it || !it.id) return;
        // 若此前有以 title 占位的合成 key，合并并删除占位。
        const ph = PLACEHOLDER_PREFIX + String(it.title || '').trim();
        if (_intents.has(ph) && !_intents.has(it.id)) {
            _intents.delete(ph);
        }
        const prev = _intents.get(it.id) || {};
        _intents.set(it.id, {
            id: it.id,
            title: it.title != null ? it.title : prev.title,
            body: it.body || prev.body || '',
            priority: it.priority != null ? it.priority : prev.priority,
            // 事件状态更实时；REST 仅在事件未覆盖时补全。
            status: prev.status || it.status || 'open',
            claimedBy: it.claimed_by || prev.claimedBy || '',
            resultFactKey: it.result_fact_key || prev.resultFactKey || '',
            resultSummary: it.result_summary || prev.resultSummary || '',
        });
    }

    function seedFromProject(projectId) {
        projectId = String(projectId || '').trim();
        if (!projectId || _seededFor === projectId) return;
        if (typeof global.apiFetch !== 'function') return;
        _seededFor = projectId;
        const base = '/api/projects/' + encodeURIComponent(projectId);

        global.apiFetch(base + '/facts?exclude_deprecated=false&limit=200')
            .then(function (r) { return r.ok ? r.json() : null; })
            .then(function (list) {
                if (Array.isArray(list)) { list.forEach(mergeFact); scheduleRender(); }
            }).catch(function () { /* 历史播种失败不影响实时 */ });

        global.apiFetch(base + '/intents')
            .then(function (r) { return r.ok ? r.json() : null; })
            .then(function (res) {
                const list = res && Array.isArray(res.intents) ? res.intents : [];
                list.forEach(mergeIntent); scheduleRender();
            }).catch(function () { /* ignore */ });

        global.apiFetch(base + '/hints')
            .then(function (r) { return r.ok ? r.json() : null; })
            .then(function (res) {
                const list = res && Array.isArray(res.hints) ? res.hints : [];
                list.forEach(function (h) {
                    if (h && h.id) _hints.set(h.id, { id: h.id, content: h.content || '' });
                });
                scheduleRender();
            }).catch(function () { /* ignore */ });
    }
    // ---- 抽屉开关 ----
    function open() {
        const root = el('blackboard-panel');
        if (!root) return;
        _open = true;
        root.classList.add('bb-open');
        root.setAttribute('aria-hidden', 'false');
        const btn = el('blackboard-toggle-btn');
        if (btn) btn.setAttribute('aria-expanded', 'true');
        // 打开时若已知 projectId 但尚未播种，则补播种。
        if (_projectId) seedFromProject(_projectId);
        else {
            const pid = typeof global.getActiveProjectId === 'function' ? (global.getActiveProjectId() || '') : '';
            if (pid) { _projectId = pid; seedFromProject(pid); }
        }
        render();
    }

    function close() {
        const root = el('blackboard-panel');
        if (!root) return;
        _open = false;
        root.classList.remove('bb-open');
        root.setAttribute('aria-hidden', 'true');
        const btn = el('blackboard-toggle-btn');
        if (btn) btn.setAttribute('aria-expanded', 'false');
    }

    function toggle() { _open ? close() : open(); }

    // reset 在新一轮黑板运行开始时清空状态（保留 projectId 以便复用播种）。
    function reset() {
        _facts.clear();
        _intents.clear();
        _hints.clear();
        _phase = '';
        _round = 0;
        _active = false;
        _seededFor = '';
        updateToggleActivity();
        scheduleRender();
    }

    // 让 toggle 按钮在有运行时显现“活跃”态（脉冲点）。
    function updateToggleActivity() {
        const btn = el('blackboard-toggle-btn');
        if (!btn) return;
        btn.classList.toggle('bb-toggle-active', !!_active);
        btn.style.display = '';
    }

    // 展示 toggle 按钮（黑板模式被选中时由 chat.js 调用）。
    function showToggle(show) {
        const btn = el('blackboard-toggle-btn');
        if (!btn) return;
        btn.style.display = show ? '' : (_active ? '' : 'none');
    }
    // ---- 人类提示注入（Cairn Hint）：POST /api/projects/:id/hints ----
    function injectHint() {
        const input = el('bb-hint-input');
        const statusEl = el('bb-hint-status');
        if (!input) return;
        const content = String(input.value || '').trim();
        if (!content) return;

        let pid = _projectId;
        if (!pid && typeof global.getActiveProjectId === 'function') pid = global.getActiveProjectId() || '';
        if (!pid) {
            if (statusEl) statusEl.textContent = tt('chat.bbHintNoProject', '需先绑定项目或启动一次黑板运行');
            return;
        }
        if (typeof global.apiFetch !== 'function') return;

        const btn = el('bb-hint-send-btn');
        if (btn) btn.disabled = true;
        if (statusEl) statusEl.textContent = tt('chat.bbHintSending', '注入中…');

        global.apiFetch('/api/projects/' + encodeURIComponent(pid) + '/hints', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ content: content, conversation_id: _conversationId || '' }),
        }).then(function (r) {
            return r.ok ? r.json() : r.json().then(function (e) { throw new Error(e && e.error ? e.error : 'HTTP ' + r.status); });
        }).then(function (h) {
            if (h && h.id) _hints.set(h.id, { id: h.id, content: h.content || content });
            input.value = '';
            if (statusEl) statusEl.textContent = tt('chat.bbHintSent', '已注入，worker 下次读板时吸收');
            scheduleRender();
            setTimeout(function () { if (statusEl) statusEl.textContent = ''; }, 4000);
        }).catch(function (err) {
            if (statusEl) statusEl.textContent = tt('chat.bbHintFailed', '注入失败：') + (err && err.message ? err.message : '');
        }).finally(function () {
            if (btn) btn.disabled = false;
        });
    }

    function bindControls() {
        const closeBtn = el('bb-close-btn');
        if (closeBtn) closeBtn.addEventListener('click', close);
        const toggleBtn = el('blackboard-toggle-btn');
        if (toggleBtn) toggleBtn.addEventListener('click', toggle);
        const sendBtn = el('bb-hint-send-btn');
        if (sendBtn) sendBtn.addEventListener('click', injectHint);
        const input = el('bb-hint-input');
        if (input) {
            input.addEventListener('keydown', function (e) {
                if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); injectHint(); }
            });
        }
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', bindControls);
    } else {
        bindControls();
    }

    // ---- 公共 API ----
    global.BlackboardPanel = {
        handleBoardEvent: handleBoardEvent,
        open: open,
        close: close,
        toggle: toggle,
        reset: reset,
        showToggle: showToggle,
        seedFromProject: seedFromProject,
        isActive: function () { return _active; },
    };
})(window);
