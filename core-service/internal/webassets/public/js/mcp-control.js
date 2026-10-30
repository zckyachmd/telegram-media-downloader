import { api } from './api.js';
import { escapeHtml, showToast } from './utils.js';
import { openSheet } from './sheet.js';

const SCOPES = [
    'monitor.read', 'monitor.write',
    'queue.read', 'queue.retry', 'queue.retry_all',
    'forward.read', 'forward.retry',
    'group.read', 'group.write',
    'topic.read', 'topic.write',
    'message.read', 'message.send', 'message.edit',
    'approval.read', 'approval.resolve',
];

const state = {
    root: null,
    agents: [],
    approvals: [],
    audit: [],
    roles: {},
    accounts: [],
    chats: [],
    errors: {},
    editingId: '',
    oneTimeSecret: null,
    busy: '',
    loading: false,
    activeView: 'agents',
};

const $ = (selector) => state.root?.querySelector(selector);
const pretty = (value) => {
    try {
        return JSON.stringify(value, null, 2);
    } catch {
        return String(value ?? '');
    }
};
const timeLabel = (seconds) => {
    const value = Number(seconds);
    return value ? new Date(value * 1000).toLocaleString() : '—';
};
const ids = (values) => (Array.isArray(values) ? values.map(String) : []);

function checkedScopes(scopes) {
    const active = new Set(scopes || []);
    return SCOPES.map((scope) => `
        <label class="flex min-h-11 min-w-0 items-center gap-2 text-xs text-tg-text bg-tg-bg/40 hover:bg-tg-bg/60 rounded-lg px-3 py-2.5 transition-colors">
            <input type="checkbox" name="mcp-scope" value="${scope}" ${active.has(scope) ? 'checked' : ''}>
            <code class="min-w-0 break-all">${scope}</code>
        </label>`).join('');
}

function selectOptions(items, selected = []) {
    const active = new Set(selected.map(String));
    if (!items.length) return '<option disabled>No items loaded; enter IDs below or refresh</option>';
    return items.map((item) => {
        const id = String(item.id ?? '');
        const name = String(item.name || id);
        const meta = item.username ? ` · @${item.username}` : item.phone ? ` · ${item.phone}` : '';
        return `<option value="${escapeHtml(id)}" ${active.has(id) ? 'selected' : ''}>${escapeHtml(name)}${escapeHtml(meta)} · ${escapeHtml(id)}</option>`;
    }).join('');
}

function grantFields(kind, label, options, selected = [], defaultAll = false) {
    const all = defaultAll || selected.includes('*');
    const available = new Set(options.map((item) => String(item.id)));
    const chosen = all ? [] : selected.filter((id) => available.has(id));
    const manual = all ? [] : selected.filter((id) => !available.has(id));
    const singular = kind === 'account' ? 'account' : 'chat';
    const allText = kind === 'account' ? 'All connected accounts, including future ones' : 'All chats, including future ones';
    const accessHelp = all
        ? `New ${kind === 'account' ? 'accounts' : 'chats'} are included automatically.`
        : `No selected IDs means this agent has no ${singular} access.`;
    const size = Math.min(Math.max(options.length, 2), 5);
    return `
        <fieldset class="space-y-2.5 min-w-0">
            <legend class="text-tg-text text-sm font-medium mb-1">${label}</legend>
            <label class="flex min-h-11 items-center gap-2 text-xs text-tg-textSecondary">
                <input type="checkbox" data-mcp-all="${kind}" ${all ? 'checked' : ''}>
                <span>${allText} (<code>*</code>)</span>
            </label>
            <select class="tg-input w-full text-sm" data-mcp-select="${kind}" multiple size="${size}" ${all ? 'disabled' : ''} aria-label="${label}" aria-describedby="mcp-${kind}-help">${selectOptions(options, chosen)}</select>
            <label class="block text-xs text-tg-textSecondary">Additional ${singular} IDs, comma separated
                <input class="tg-input w-full text-sm mt-1.5" data-mcp-manual="${kind}" value="${escapeHtml(manual.join(', '))}" placeholder="12345, -1001234567890" ${all ? 'disabled' : ''}>
            </label>
            <p id="mcp-${kind}-help" class="text-xs text-tg-textSecondary">${accessHelp}${state.errors[kind === 'account' ? 'accounts' : 'chats'] ? ` Resource list failed to load; enter IDs manually. (${escapeHtml(state.errors[kind === 'account' ? 'accounts' : 'chats'])})` : ''}</p>
        </fieldset>`;
}

function agentEditor({ dialog = false } = {}) {
    const editing = state.agents.find((agent) => agent.id === state.editingId);
    const role = editing?.role || 'read-only';
    const scopes = editing?.scopes || state.roles[role] || [];
    const roleOptions = ['read-only', 'operator', 'admin', 'custom'].map((name) =>
        `<option value="${name}" ${role === name ? 'selected' : ''}>${name}</option>`,
    ).join('');
    return `
        <form id="mcp-agent-form" class="${dialog ? 'mcp-agent-form-dialog' : ''} space-y-5 pb-2">
                    ${editing && !dialog ? '<div class="flex justify-end"><button type="button" class="tg-btn-secondary min-h-11 px-3 py-2 text-xs" data-mcp-action="cancel-edit">Cancel editing</button></div>' : ''}
                    ${editing ? '<p class="text-xs text-tg-textSecondary">Changing permissions revokes active tokens.</p>' : ''}
                    <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                        <label class="text-xs text-tg-textSecondary">Agent name
                            <input class="tg-input w-full text-sm mt-1.5" name="name" required maxlength="100" value="${escapeHtml(editing?.name || '')}" placeholder="e.g. queue-operator">
                        </label>
                        <label class="text-xs text-tg-textSecondary">Role
                            <select class="tg-input w-full text-sm mt-1.5" name="role">${roleOptions}</select>
                        </label>
                    </div>
                    <div class="grid grid-cols-1 lg:grid-cols-2 gap-5">
                        ${grantFields('account', 'Telegram accounts', state.accounts, editing ? ids(editing.accountIds) : [], !editing)}
                        ${grantFields('chat', 'Chats', state.chats, editing ? ids(editing.chatIds) : [], !editing)}
                    </div>
                    <fieldset class="space-y-2">
                        <legend class="text-sm text-tg-text font-medium">Tool scopes</legend>
                        <p class="text-xs text-tg-textSecondary">Choose which operations this identity can request.</p>
                        <div class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-4 gap-2">${checkedScopes(scopes)}</div>
                    </fieldset>
                    <div class="mcp-agent-submit flex items-center gap-3 flex-wrap border-t border-tg-border/40 pt-4">
                        <button type="submit" class="tg-btn-input min-h-11 text-sm px-4 py-2.5" ${state.busy ? 'disabled' : ''}>${state.busy ? 'Saving…' : editing ? 'Save permissions' : 'Create agent'}</button>
                        <span class="text-xs text-tg-textSecondary">Each identity has a separate client ID and revocable secret.</span>
                    </div>
        </form>`;
}

function openAgentSheet(agentId = '') {
    const editing = Boolean(agentId);
    if (editing && !state.agents.some((agent) => agent.id === agentId)) return;
    state.editingId = agentId;
    const sheet = openSheet({
        title: editing ? 'Edit MCP agent' : 'Create MCP agent',
        content: agentEditor({ dialog: true }),
        size: 'lg',
        onClose: () => { state.editingId = ''; },
    });
    const form = sheet.body.querySelector('#mcp-agent-form');
    if (!form) return sheet.close();
    bindFormDefaults(form);
    form.addEventListener('submit', (event) => saveAgent(event, () => sheet.close()));
}

function openCreateAgentSheet() {
    openAgentSheet();
}

function openEditAgentSheet(id) {
    openAgentSheet(id);
}

function secretPanel() {
    if (!state.oneTimeSecret) return '';
    const secret = state.oneTimeSecret;
    return `
        <section class="border border-tg-orange/50 bg-tg-orange/10 rounded-lg p-3 mb-4" aria-labelledby="mcp-secret-title">
            <div class="flex items-start gap-2">
                <i class="ri-alert-line text-tg-orange mt-0.5" aria-hidden="true"></i>
                <div class="min-w-0 flex-1">
                    <h4 id="mcp-secret-title" class="text-sm text-tg-text font-semibold">Save this credential now</h4>
                    <p class="text-xs text-tg-textSecondary mt-1">The client secret is shown once. Store it in the agent’s secret manager; it cannot be retrieved later.</p>
                    <dl class="grid grid-cols-1 gap-2 mt-3 text-xs">
                        <div><dt class="text-tg-textSecondary">Client ID</dt><dd><code class="break-all select-all">${escapeHtml(secret.clientId)}</code></dd></div>
                        <div><dt class="text-tg-textSecondary">Client secret</dt><dd><code class="break-all select-all font-semibold">${escapeHtml(secret.clientSecret)}</code></dd></div>
                    </dl>
                </div>
                <button type="button" class="tg-btn-secondary min-h-11 text-xs px-3 py-2 shrink-0" data-mcp-action="dismiss-secret">Dismiss</button>
            </div>
        </section>`;
}

function emptyState(icon, title, description) {
    return `
        <div class="flex items-center gap-3 py-3 text-tg-textSecondary">
            <i class="${icon} shrink-0 text-lg leading-none text-tg-blue" aria-hidden="true"></i>
            <div class="min-w-0">
                <p class="text-sm font-medium text-tg-text">${escapeHtml(title)}</p>
                ${description ? `<p class="mt-0.5 text-xs leading-relaxed">${escapeHtml(description)}</p>` : ''}
            </div>
        </div>`;
}

function errorState(title, message) {
    return `
        <div class="flex items-start gap-3 rounded-lg border border-tg-red/30 bg-tg-red/10 p-3" role="alert">
            <i class="ri-error-warning-line mt-0.5 text-lg text-tg-red" aria-hidden="true"></i>
            <div class="min-w-0 flex-1">
                <p class="text-sm font-medium text-tg-text">${escapeHtml(title)} could not be loaded</p>
                <p class="mt-0.5 break-words text-xs text-tg-textSecondary">${escapeHtml(message)}</p>
            </div>
            <button type="button" class="tg-btn-secondary min-h-11 shrink-0 px-3 py-2 text-xs" data-mcp-action="retry-load">Retry</button>
        </div>`;
}

function overview() {
    const activeAgents = state.agents.filter((agent) => agent.active === true).length;
    return `<p class="mb-3 inline-flex min-h-9 items-center gap-2 rounded-full bg-tg-bg/50 px-3 py-1.5 text-xs text-tg-textSecondary" role="status" aria-live="polite"><i class="ri-shield-check-line text-tg-blue" aria-hidden="true"></i>${state.errors.agents ? 'Agent status unavailable' : `${activeAgents} active agent${activeAgents === 1 ? '' : 's'}`}${state.errors.approvals ? ' · approvals unavailable' : state.approvals.length ? ` · ${state.approvals.length} awaiting approval` : ''}</p>`;
}

function agentRows() {
    const host = $('#mcp-agent-list');
    if (!host) return;
    if (state.errors.agents) {
        host.innerHTML = errorState('Agent identities', state.errors.agents);
        return;
    }
    if (!state.agents.length) {
        host.innerHTML = emptyState('ri-shield-user-line', 'No agents yet', '');
        return;
    }
    host.innerHTML = state.agents.map((agent) => {
        const active = agent.active === true;
        const accounts = ids(agent.accountIds);
        const chats = ids(agent.chatIds);
        const scopeText = ids(agent.scopes).join(', ') || 'No scopes';
        const stateClass = active
            ? 'bg-tg-green/10 text-tg-green'
            : 'bg-tg-bg/50 text-tg-textSecondary';
        return `
            <article class="rounded-xl border border-tg-border/50 bg-tg-bg/20 p-4 ${active ? '' : 'opacity-75'}">
                <div class="flex items-start justify-between gap-3 flex-wrap">
                    <div class="min-w-0 flex-1">
                        <div class="flex items-center gap-2 flex-wrap">
                            <h5 class="text-base font-semibold text-tg-text">${escapeHtml(agent.name)}</h5>
                            <span class="rounded-full bg-tg-bg/60 px-2.5 py-1 text-[11px] font-medium text-tg-textSecondary">${escapeHtml(agent.role)}</span>
                            <span class="rounded-full px-2.5 py-1 text-[11px] font-medium ${stateClass}">${active ? 'Active' : 'Revoked'}</span>
                        </div>
                        <p class="mt-1.5 break-all text-xs text-tg-textSecondary">Client ID <code class="select-all text-tg-text">${escapeHtml(agent.clientId)}</code><span class="mx-1">·</span>Updated ${timeLabel(agent.updatedAt)}</p>
                    </div>
                    <div class="flex items-center gap-2 flex-wrap">
                        ${active ? `
                            <button type="button" class="tg-btn-secondary min-h-11 px-3 py-2 text-xs" data-mcp-action="edit-agent" data-id="${escapeHtml(agent.id)}">Edit access</button>
                            <button type="button" class="tg-btn-secondary min-h-11 px-3 py-2 text-xs" data-mcp-action="rotate-agent" data-id="${escapeHtml(agent.id)}">Rotate secret</button>
                            <button type="button" class="tg-btn-secondary min-h-11 px-3 py-2 text-xs text-tg-red" data-mcp-action="revoke-agent" data-id="${escapeHtml(agent.id)}">Revoke</button>
                        ` : ''}
                    </div>
                </div>
                <div class="mt-4 grid grid-cols-1 gap-3 border-t border-tg-border/40 pt-3 sm:grid-cols-3 sm:gap-4 text-xs">
                    <div class="min-w-0"><span class="block text-tg-textSecondary">Telegram accounts</span><code class="mt-1 block break-all text-tg-text">${escapeHtml(accounts.join(', ') || 'None')}</code></div>
                    <div class="min-w-0"><span class="block text-tg-textSecondary">Chats</span><code class="mt-1 block break-all text-tg-text">${escapeHtml(chats.join(', ') || 'None')}</code></div>
                    <details class="min-w-0">
                        <summary class="min-h-8 cursor-pointer text-tg-blue">${ids(agent.scopes).length} tool scopes</summary>
                        <p class="mt-1 break-words leading-relaxed text-tg-textSecondary">${escapeHtml(scopeText)}</p>
                    </details>
                </div>
            </article>`;
    }).join('');
}

function approvalRows() {
    const host = $('#mcp-approval-list');
    if (!host) return;
    if (state.errors.approvals) {
        host.innerHTML = errorState('Pending approvals', state.errors.approvals);
        return;
    }
    if (!state.approvals.length) {
        host.innerHTML = emptyState('ri-checkbox-circle-line', 'Nothing to review', 'Pending requests will appear here.');
        return;
    }
    const names = new Map(state.agents.map((agent) => [agent.id, agent.name]));
    host.innerHTML = state.approvals.map((item) => `
        <article class="rounded-xl border border-tg-border/50 bg-tg-bg/20 p-4">
            <div class="flex items-start justify-between gap-3 flex-wrap">
                <div class="min-w-0 flex-1">
                    <div class="flex gap-2 flex-wrap items-center">
                        <strong class="text-sm text-tg-text">${escapeHtml(item.operation)}</strong>
                        <span class="rounded-full bg-tg-orange/10 px-2.5 py-1 text-[11px] font-medium text-tg-orange">Needs review</span>
                    </div>
                    <code class="mt-2 block break-all text-xs text-tg-textSecondary">${escapeHtml(item.target)}</code>
                    <p class="text-sm text-tg-text mt-2">${escapeHtml(item.summary)}</p>
                    <p class="text-xs text-tg-textSecondary mt-2">Requested by <strong class="font-medium text-tg-text">${escapeHtml(names.get(item.requesterAgentId) || item.requesterAgentId)}</strong><span class="mx-1">·</span>Expires ${timeLabel(item.expiresAt)}</p>
                </div>
            </div>
            <details class="mt-4 border-t border-tg-border/40 pt-3">
                <summary class="min-h-8 cursor-pointer text-xs font-medium text-tg-blue">Inspect exact request payload</summary>
                <pre class="mt-2 max-h-64 overflow-auto rounded-lg bg-tg-bg p-3 text-xs leading-relaxed whitespace-pre-wrap break-all">${escapeHtml(pretty(item.payload))}</pre>
                <p class="mt-2 break-all text-xs text-tg-textSecondary">Payload digest <code class="select-all text-tg-text">${escapeHtml(item.payloadHash)}</code></p>
            </details>
            <div class="mt-3 flex flex-col gap-3 border-t border-tg-border/40 pt-3 sm:flex-row sm:items-center sm:justify-between">
                <label class="flex min-h-11 items-center gap-2 text-xs text-tg-textSecondary">
                    <input type="checkbox" data-mcp-reviewed="${escapeHtml(item.id)}">
                    I have reviewed this payload
                </label>
                <div class="flex w-full flex-col-reverse gap-2 sm:w-auto sm:flex-row">
                    <button type="button" class="tg-btn-secondary min-h-11 w-full px-4 py-2 text-sm sm:w-auto" data-mcp-action="decide-approval" data-decision="rejected" data-id="${escapeHtml(item.id)}" ${state.busy === item.id ? 'disabled' : ''}>Reject</button>
                    <button type="button" class="tg-btn-input min-h-11 w-full px-4 py-2 text-sm sm:w-auto" data-mcp-action="decide-approval" data-decision="approved" data-id="${escapeHtml(item.id)}" disabled>Approve and execute</button>
                </div>
            </div>
        </article>`).join('');
}

function auditRows() {
    const host = $('#mcp-audit-list');
    if (!host) return;
    if (state.errors.audit) {
        host.innerHTML = errorState('Audit trail', state.errors.audit);
        return;
    }
    if (!state.audit.length) {
        host.innerHTML = emptyState('ri-file-list-3-line', 'No audit events yet', 'Agent changes, MCP requests, and approval decisions will be recorded here.');
        return;
    }
    host.innerHTML = state.audit.map((item) => {
        const outcome = item.outcome || '—';
        const tone = outcome === 'succeeded'
            ? 'bg-tg-green/10 text-tg-green'
            : ['failed', 'rejected', 'denied', 'error'].includes(outcome)
                ? 'bg-tg-red/10 text-tg-red'
                : 'bg-tg-bg/50 text-tg-textSecondary';
        return `
        <article class="grid grid-cols-1 gap-x-4 gap-y-2 border-b border-tg-border/40 py-3 text-xs sm:grid-cols-[10rem_minmax(0,1fr)]">
            <div class="flex flex-wrap items-center justify-between gap-2 sm:block">
                <time class="tabular-nums text-tg-textSecondary">${escapeHtml(timeLabel(item.occurredAt))}</time>
                <span class="rounded-full px-2.5 py-1 text-[11px] font-medium ${tone}">${escapeHtml(outcome)}</span>
            </div>
            <div class="min-w-0">
                <p class="break-all text-tg-textSecondary">${escapeHtml(item.actorType)} · ${escapeHtml(item.actorId)}</p>
                <p class="mt-1"><strong class="font-semibold text-tg-text">${escapeHtml(item.action)}</strong><span class="text-tg-textSecondary"> · ${escapeHtml(item.summary || '')}</span></p>
                ${item.target ? `<p class="mt-1 break-all text-tg-textSecondary">Target <code class="text-tg-text">${escapeHtml(item.target)}</code></p>` : ''}
                ${item.payloadHash ? `<p class="mt-1 break-all text-tg-textSecondary">Payload SHA-256 <code class="text-tg-text">${escapeHtml(item.payloadHash.slice(0, 16))}…</code></p>` : ''}
            </div>
        </article>`;
    }).join('');
}

function renderLists() {
    agentRows();
    approvalRows();
    auditRows();
    $('#mcp-agent-count')?.replaceChildren(document.createTextNode(state.errors.agents ? '—' : String(state.agents.length)));
    $('#mcp-approval-count')?.replaceChildren(document.createTextNode(state.errors.approvals ? '—' : String(state.approvals.length)));
    $('#mcp-audit-count')?.replaceChildren(document.createTextNode(state.errors.audit ? '—' : String(state.audit.length)));
}

function render() {
    if (!state.root) return;
    state.root.innerHTML = `
        ${secretPanel()}
        ${overview()}
        <div class="mcp-access-tabs" role="tablist" aria-label="MCP access sections">
            <button type="button" role="tab" id="mcp-tab-agents" aria-controls="mcp-panel-agents" data-mcp-view="agents" class="mcp-access-tab">Agents <span id="mcp-agent-count" class="mcp-access-count">${state.agents.length}</span></button>
            <button type="button" role="tab" id="mcp-tab-approvals" aria-controls="mcp-panel-approvals" data-mcp-view="approvals" class="mcp-access-tab">Approvals <span id="mcp-approval-count" class="mcp-access-count">${state.approvals.length}</span></button>
            <button type="button" role="tab" id="mcp-tab-audit" aria-controls="mcp-panel-audit" data-mcp-view="audit" class="mcp-access-tab">Audit <span id="mcp-audit-count" class="mcp-access-count">${state.audit.length}</span></button>
        </div>
        <section id="mcp-panel-agents" role="tabpanel" tabindex="0" aria-labelledby="mcp-tab-agents" class="mcp-scroll-panel" ${state.activeView !== 'agents' ? 'hidden' : ''}>
            <div id="mcp-agent-list" class="space-y-3"></div>
        </section>
        <section id="mcp-panel-approvals" role="tabpanel" tabindex="0" aria-labelledby="mcp-tab-approvals" class="mcp-scroll-panel" ${state.activeView !== 'approvals' ? 'hidden' : ''}>
            <div id="mcp-approval-list" class="space-y-3"></div>
        </section>
        <section id="mcp-panel-audit" role="tabpanel" tabindex="0" aria-labelledby="mcp-tab-audit" class="mcp-scroll-panel" ${state.activeView !== 'audit' ? 'hidden' : ''}>
            <div id="mcp-audit-list" class="border-t border-tg-border/40"></div>
        </section>`;
    for (const tab of state.root.querySelectorAll('[data-mcp-view]')) {
        const selected = tab.dataset.mcpView === state.activeView;
        tab.setAttribute('aria-selected', String(selected));
        tab.tabIndex = selected ? 0 : -1;
    }
    const createButton = document.getElementById('mcp-create-agent');
    if (createButton) createButton.hidden = state.activeView !== 'agents' || Boolean(state.editingId);
    renderLists();
}

function bindFormDefaults(form = $('#mcp-agent-form')) {
    if (!form) return;
    form.querySelector('[name="role"]')?.addEventListener('change', (event) => {
        const role = event.target.value;
        if (role === 'custom') return;
        const scopes = new Set(state.roles[role] || []);
        form.querySelectorAll('[name="mcp-scope"]').forEach((input) => {
            input.checked = scopes.has(input.value);
        });
    });
    form.querySelectorAll('[data-mcp-all]').forEach((input) => {
        input.addEventListener('change', () => {
            const kind = input.dataset.mcpAll;
            form.querySelector(`[data-mcp-select="${kind}"]`).disabled = input.checked;
            form.querySelector(`[data-mcp-manual="${kind}"]`).disabled = input.checked;
        });
    });
}

function readGrant(kind, form) {
    if (form.querySelector(`[data-mcp-all="${kind}"]`)?.checked) return ['*'];
    const selected = Array.from(form.querySelector(`[data-mcp-select="${kind}"]`)?.selectedOptions || []).map((option) => option.value);
    const manual = form.querySelector(`[data-mcp-manual="${kind}"]`)?.value || '';
    const extra = manual.split(',').map((value) => value.trim()).filter(Boolean);
    return [...new Set([...selected, ...extra])];
}

function setGrantReview(id, checked) {
    const button = state.root.querySelector(`[data-mcp-action="decide-approval"][data-id="${CSS.escape(id)}"][data-decision="approved"]`);
    if (button) button.disabled = !checked || state.busy === id;
}

function updateRefreshButton() {
    const button = $('#mcp-refresh');
    state.root?.setAttribute('aria-busy', String(state.loading));
    state.root?.querySelectorAll('[data-mcp-action="retry-load"]').forEach((retry) => {
        retry.disabled = state.loading;
        retry.textContent = state.loading ? 'Retrying…' : 'Retry';
    });
    if (!button) return;
    button.disabled = state.loading;
    button.setAttribute('aria-busy', String(state.loading));
    const label = button.querySelector('span');
    if (label) label.textContent = state.loading ? 'Refreshing…' : 'Refresh';
}

async function refresh() {
    state.loading = true;
    updateRefreshButton();
    state.errors = {};
    try {
        const requests = await Promise.allSettled([
            api.get('/api/mcp/agents'),
            api.get('/api/mcp/approvals'),
            api.get('/api/mcp/audit?limit=100'),
            api.get('/api/accounts'),
            api.get('/api/groups'),
        ]);
        const [agents, approvals, audit, accounts, chats] = requests;
        if (agents.status === 'fulfilled') {
            state.agents = agents.value.agents || [];
            state.roles = agents.value.roles || {};
        } else state.errors.agents = agents.reason?.message || String(agents.reason);
        if (approvals.status === 'fulfilled') state.approvals = approvals.value.approvals || [];
        else state.errors.approvals = approvals.reason?.message || String(approvals.reason);
        if (audit.status === 'fulfilled') state.audit = audit.value.entries || [];
        else state.errors.audit = audit.reason?.message || String(audit.reason);
        if (accounts.status === 'fulfilled') state.accounts = Array.isArray(accounts.value) ? accounts.value : [];
        else state.errors.accounts = accounts.reason?.message || String(accounts.reason);
        if (chats.status === 'fulfilled') state.chats = Array.isArray(chats.value) ? chats.value : [];
        else state.errors.chats = chats.reason?.message || String(chats.reason);
    } finally {
        state.loading = false;
        updateRefreshButton();
        renderLists();
    }
}

function reportError(error) {
    const message = error?.data?.error || error?.message || 'MCP control action failed';
    showToast(message, 'error');
}

async function saveAgent(event, onSuccess) {
    event.preventDefault();
    const form = event.target;
    const role = form.elements.role.value;
    const scopes = Array.from(form.querySelectorAll('[name="mcp-scope"]:checked')).map((input) => input.value);
    if (role !== 'custom' && scopes.length === 0) {
        showToast('Select at least one scope, or choose the custom role for an agent with no tool access.', 'warning');
        return;
    }
    const input = {
        name: form.elements.name.value.trim(),
        role,
        scopes,
        accountIds: readGrant('account', form),
        chatIds: readGrant('chat', form),
    };
    state.busy = 'save';
    form.querySelector('[type="submit"]').disabled = true;
    try {
        if (state.editingId) {
            await api.patch(`/api/mcp/agents/${encodeURIComponent(state.editingId)}`, input);
            showToast('Agent permissions saved; active tokens were revoked.', 'success');
            state.editingId = '';
        } else {
            const result = await api.post('/api/mcp/agents', input);
            state.oneTimeSecret = {
                clientId: result.agent.clientId,
                clientSecret: result.clientSecret,
            };
            showToast('Agent created. Save the one-time client secret.', 'success');
        }
        state.busy = '';
        onSuccess?.();
        await refresh();
        render();
    } catch (error) {
        state.busy = '';
        form.querySelector('[type="submit"]').disabled = false;
        reportError(error);
    }
}

async function decideApproval(id, decision) {
    const item = state.approvals.find((approval) => approval.id === id);
    if (!item) return;
    if (decision === 'approved' && !state.root.querySelector(`[data-mcp-reviewed="${CSS.escape(id)}"]`)?.checked) return;
    const verb = decision === 'approved' ? 'approve and execute' : 'reject';
    if (!window.confirm(`${verb[0].toUpperCase()}${verb.slice(1)} ${item.operation} for ${item.target}?\n\n${item.summary}`)) return;
    state.busy = id;
    try {
        await api.post(`/api/mcp/approvals/${encodeURIComponent(id)}/decision`, { decision });
        showToast(decision === 'approved' ? 'Approval accepted; operation executed.' : 'Operation rejected.', 'success');
    } catch (error) {
        reportError(error);
    } finally {
        state.busy = '';
        await refresh();
    }
}

async function handleClick(event) {
    const tab = event.target.closest('[data-mcp-view]');
    if (tab) {
        state.activeView = tab.dataset.mcpView;
        render();
        $(`#mcp-tab-${state.activeView}`)?.focus();
        return;
    }
    const button = event.target.closest('[data-mcp-action]');
    if (!button) return;
    const { mcpAction: action, id } = button.dataset;
    if (action === 'retry-load') {
        try { await refresh(); } catch (error) { reportError(error); }
        return;
    }
    if (action === 'cancel-edit') {
        state.editingId = '';
        render();
        return;
    }
    if (action === 'dismiss-secret') {
        state.oneTimeSecret = null;
        render();
        return;
    }
    if (action === 'edit-agent') {
        state.activeView = 'agents';
        openEditAgentSheet(id);
        return;
    }
    if (action === 'rotate-agent') {
        const agent = state.agents.find((item) => item.id === id);
        if (!window.confirm(`Rotate the secret for ${agent?.name || id}? Existing tokens will be revoked.`)) return;
        try {
            const result = await api.post(`/api/mcp/agents/${encodeURIComponent(id)}/rotate-secret`, {});
            state.oneTimeSecret = { clientId: agent?.clientId || '', clientSecret: result.clientSecret };
            showToast('Secret rotated. Save the new one-time secret.', 'success');
            await refresh();
            render();
        } catch (error) { reportError(error); }
        return;
    }
    if (action === 'revoke-agent') {
        const agent = state.agents.find((item) => item.id === id);
        if (!window.confirm(`Revoke ${agent?.name || id}? This disables the identity and revokes its active tokens.`)) return;
        try {
            await api.delete(`/api/mcp/agents/${encodeURIComponent(id)}`);
            if (state.editingId === id) state.editingId = '';
            showToast('Agent revoked.', 'success');
            await refresh();
        } catch (error) { reportError(error); }
        return;
    }
    if (action === 'decide-approval') await decideApproval(id, button.dataset.decision);
}

export async function initMCPControl() {
    const root = document.getElementById('mcp-control');
    if (!root) return;
    if (root.dataset.mcpBound !== 'true') {
        state.root = root;
        root.dataset.mcpBound = 'true';
        root.addEventListener('submit', (event) => {
            if (event.target.id === 'mcp-agent-form') saveAgent(event);
        });
        root.addEventListener('click', handleClick);
        root.addEventListener('keydown', (event) => {
            const current = event.target.closest('[data-mcp-view]');
            if (!current) return;
            const tabs = Array.from(root.querySelectorAll('[data-mcp-view]'));
            const index = tabs.indexOf(current);
            const next = event.key === 'ArrowRight' ? (index + 1) % tabs.length
                : event.key === 'ArrowLeft' ? (index - 1 + tabs.length) % tabs.length
                    : event.key === 'Home' ? 0
                        : event.key === 'End' ? tabs.length - 1 : -1;
            if (next < 0) return;
            event.preventDefault();
            state.activeView = tabs[next].dataset.mcpView;
            render();
            $(`#mcp-tab-${state.activeView}`)?.focus();
        });
        root.addEventListener('change', (event) => {
            const input = event.target.closest('[data-mcp-reviewed]');
            if (input) setGrantReview(input.dataset.mcpReviewed, input.checked);
        });
        document.getElementById('mcp-create-agent')?.addEventListener('click', openCreateAgentSheet);
        document.getElementById('mcp-refresh')?.addEventListener('click', () => refresh().catch(reportError));
    }
    state.root = root;
    await refresh();
    render();
}

export function clearOneTimeSecret() {
    state.oneTimeSecret = null;
    state.root?.querySelector('[aria-labelledby="mcp-secret-title"]')?.remove();
}
