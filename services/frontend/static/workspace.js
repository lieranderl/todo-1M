(() => {
  'use strict';

  const CONFIG = {
    apiBase: 'http://localhost:8080',
    queryBase: 'http://localhost:8081',
    loginURL: '/login',
    streamRetryMs: 500,
    projection: {
      initialDelayMs: 90,
      debounceMs: 120,
      maxDelayMs: 900,
      backoffFactor: 1.4,
    },
    storage: {
      access: 'todo_access_token',
      legacyAccess: 'todo_token',
      refresh: 'todo_refresh_token',
      username: 'todo_username',
      userID: 'todo_user_id',
      activeGroup: 'todo_active_group',
      showFeed: 'todo_show_feed',
    },
  };

  const els = {
    authGuard: document.getElementById('auth-guard'),
    appShell: document.getElementById('app-shell'),
    authStatus: document.getElementById('auth-status'),
    toolbarUser: document.getElementById('toolbar-user'),
    refreshBtn: document.getElementById('refresh-btn'),
    logoutBtn: document.getElementById('logout-btn'),
    groupName: document.getElementById('group-name'),
    createGroupBtn: document.getElementById('create-group-btn'),
    activeGroup: document.getElementById('active-group'),
    connectGroupBtn: document.getElementById('connect-group-btn'),
    groupsList: document.getElementById('groups-list'),
    memberUsername: document.getElementById('member-username'),
    memberRole: document.getElementById('member-role'),
    addMemberBtn: document.getElementById('add-member-btn'),
    roleUsername: document.getElementById('role-username'),
    roleValue: document.getElementById('role-value'),
    updateRoleBtn: document.getElementById('update-role-btn'),
    todoInput: document.getElementById('todo-input'),
    sendBtn: document.getElementById('send-btn'),
    todos: document.getElementById('todos'),
    feedToggle: document.getElementById('feed-visible-toggle'),
    feedCard: document.getElementById('feed-card'),
    events: document.getElementById('events'),
  };

  if (!els.appShell || !els.authGuard || !els.todoInput) {
    return;
  }

  const state = {
    auth: {
      accessToken: localStorage.getItem(CONFIG.storage.access) || localStorage.getItem(CONFIG.storage.legacyAccess) || '',
      refreshToken: localStorage.getItem(CONFIG.storage.refresh) || '',
      username: localStorage.getItem(CONFIG.storage.username) || '',
      userID: localStorage.getItem(CONFIG.storage.userID) || '',
    },
    activeGroupRole: '',
    groupsCache: [],
    stream: null,
    currentTodos: [],
    editingTodoID: '',
    projection: {
      pendingEventSeq: 0,
      timer: null,
      running: false,
      delayMs: CONFIG.projection.initialDelayMs,
    },
  };

  const escapeHTML = (value) => String(value || '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');

  const redirectToLogin = () => {
    window.location.href = CONFIG.loginURL;
  };

  const setStatus = (message, isError = false) => {
    els.authStatus.textContent = message;
    els.authStatus.className = isError ? 'text-lg font-bold text-error' : 'text-lg font-bold text-success';
  };

  const persistAuth = (payload) => {
    state.auth = {
      accessToken: payload.access_token || payload.token || '',
      refreshToken: payload.refresh_token || '',
      username: payload.username || '',
      userID: payload.user_id || '',
    };

    localStorage.setItem(CONFIG.storage.access, state.auth.accessToken);
    localStorage.setItem(CONFIG.storage.legacyAccess, state.auth.accessToken);
    localStorage.setItem(CONFIG.storage.refresh, state.auth.refreshToken || '');
    localStorage.setItem(CONFIG.storage.username, state.auth.username || '');
    localStorage.setItem(CONFIG.storage.userID, state.auth.userID || '');

    els.toolbarUser.textContent = state.auth.username || 'user';
  };

  const clearAuth = () => {
    state.auth = { accessToken: '', refreshToken: '', username: '', userID: '' };
    localStorage.removeItem(CONFIG.storage.access);
    localStorage.removeItem(CONFIG.storage.legacyAccess);
    localStorage.removeItem(CONFIG.storage.refresh);
    localStorage.removeItem(CONFIG.storage.username);
    localStorage.removeItem(CONFIG.storage.userID);
  };

  const authHeaders = () => {
    const headers = { 'Content-Type': 'application/json' };
    if (state.auth.accessToken) {
      headers.Authorization = `Bearer ${state.auth.accessToken}`;
    }
    return headers;
  };

  const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

  const refreshSession = async () => {
    if (!state.auth.refreshToken) {
      throw new Error('no refresh token');
    }

    const response = await fetch(`${CONFIG.apiBase}/api/v1/auth/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refresh_token: state.auth.refreshToken }),
    });

    let payload = null;
    try {
      payload = await response.json();
    } catch (_) {
      // noop
    }

    if (!response.ok) {
      throw new Error((payload && payload.error) || 'refresh failed');
    }

    persistAuth(payload);
    return payload;
  };

  const apiRequest = async (url, options = {}, allowRetry = true) => {
    const mergedOptions = {
      ...options,
      headers: { ...(options.headers || {}), ...authHeaders() },
    };

    const response = await fetch(url, mergedOptions);
    let payload = null;
    try {
      payload = await response.json();
    } catch (_) {
      // noop
    }

    if (response.status === 401) {
      if (allowRetry && state.auth.refreshToken) {
        try {
          await refreshSession();
          return apiRequest(url, options, false);
        } catch (_) {
          clearAuth();
          redirectToLogin();
          throw new Error('session expired');
        }
      }

      clearAuth();
      redirectToLogin();
      throw new Error('session expired');
    }

    if (!response.ok) {
      throw new Error((payload && payload.error) || `request failed: ${response.status}`);
    }

    return payload;
  };

  const setRoleFromActiveGroup = () => {
    const groupID = (els.activeGroup.value || '').trim();
    const group = state.groupsCache.find((item) => (item.group_id || item.id) === groupID);
    state.activeGroupRole = group ? (group.role || '') : '';
  };

  const applyFeedVisibility = () => {
    const visible = !!els.feedToggle.checked;
    els.feedCard.style.display = visible ? 'block' : 'none';
    localStorage.setItem(CONFIG.storage.showFeed, visible ? '1' : '0');
  };

  const prependEventHTML = (html) => {
    if (!html || !els.events) {
      return;
    }
    const temp = document.createElement('div');
    temp.innerHTML = html.trim();
    const first = temp.firstElementChild;
    if (!first) {
      return;
    }

    const spinner = els.events.querySelector('.loading');
    if (spinner) {
      els.events.innerHTML = '';
    }
    els.events.prepend(first);
  };

  const canModerate = () => state.activeGroupRole === 'owner' || state.activeGroupRole === 'admin';

  const canEditTodo = (todo) => canModerate() || todo.created_by_user_id === state.auth.userID;

  const bindTodoActions = () => {
    els.todos.querySelectorAll('[data-edit]').forEach((button) => {
      button.addEventListener('click', () => {
        state.editingTodoID = button.getAttribute('data-edit') || '';
        renderTodos();
      });
    });

    els.todos.querySelectorAll('[data-cancel]').forEach((button) => {
      button.addEventListener('click', () => {
        state.editingTodoID = '';
        renderTodos();
      });
    });

    els.todos.querySelectorAll('[data-save]').forEach((button) => {
      button.addEventListener('click', async () => {
        const todoID = button.getAttribute('data-save') || '';
        const input = document.getElementById(`edit-${todoID}`);
        const title = ((input && input.value) || '').trim();
        if (!title) {
          return;
        }

        await sendCommand('update-todo', title, todoID);
        state.editingTodoID = '';
        await loadTodos();
      });
    });

    els.todos.querySelectorAll('[data-delete]').forEach((button) => {
      button.addEventListener('click', async () => {
        const todoID = button.getAttribute('data-delete') || '';
        await sendCommand('delete-todo', '', todoID);
        await loadTodos();
      });
    });
  };

  const renderTodos = () => {
    els.todos.innerHTML = '';

    if (!state.currentTodos.length) {
      els.todos.innerHTML = `
        <div class="alert border border-base-300/60 bg-base-200/70">
          <span class="font-medium">No todos yet. Add the first task for this group.</span>
        </div>
      `;
      return;
    }

    for (const todo of state.currentTodos) {
      const card = document.createElement('div');
      card.className = 'card bg-base-100 border border-base-300/60 shadow';

      const title = escapeHTML(todo.title);
      const createdBy = escapeHTML(todo.created_by_username);
      const updatedBy = escapeHTML(todo.updated_by_username);
      const meta = updatedBy && updatedBy !== createdBy
        ? `Created by ${createdBy} • Updated by ${updatedBy}`
        : `Created by ${createdBy}`;
      const todoID = String(todo.todo_id || '');
      const groupID = String(todo.group_id || '');

      if (state.editingTodoID === todoID && canEditTodo(todo)) {
        card.innerHTML = `
          <div class="card-body p-4 gap-3">
            <div class="flex items-center justify-between">
              <span class="badge badge-outline badge-info">Editing</span>
              <span class="text-[11px] text-base-content/60">Todo ID: ${escapeHTML(todoID.slice(0, 8))}</span>
            </div>
            <div class="flex gap-2">
              <input id="edit-${escapeHTML(todoID)}" class="input input-bordered w-full" value="${title}" />
              <button class="btn btn-primary btn-sm" data-save="${escapeHTML(todoID)}">Save</button>
              <button class="btn btn-sm" data-cancel="${escapeHTML(todoID)}">Cancel</button>
            </div>
            <p class="text-xs text-base-content/70">${meta}</p>
          </div>
        `;
      } else {
        const buttons = canEditTodo(todo)
          ? `<button class="btn btn-xs btn-outline" data-edit="${escapeHTML(todoID)}">Edit</button>
             <button class="btn btn-xs btn-error btn-outline" data-delete="${escapeHTML(todoID)}">Delete</button>`
          : '';

        card.innerHTML = `
          <div class="card-body p-4 gap-3">
            <div class="flex justify-between gap-2 items-start">
              <div>
                <div class="font-semibold text-base-content text-base">${title}</div>
                <div class="text-xs text-base-content/70 mt-1">${meta}</div>
              </div>
              <div class="flex gap-1">${buttons}</div>
            </div>
            <div class="divider my-0"></div>
            <div class="flex items-center justify-between text-[11px] text-base-content/60">
              <span class="badge badge-ghost badge-sm">group ${escapeHTML(groupID.slice(0, 8))}</span>
              <span>todo ${escapeHTML(todoID.slice(0, 8))}</span>
            </div>
          </div>
        `;
      }

      els.todos.appendChild(card);
    }

    bindTodoActions();
  };

  const loadGroups = async () => {
    const data = await apiRequest(`${CONFIG.queryBase}/api/v1/groups`, { method: 'GET', headers: {} });
    state.groupsCache = data.groups || [];

    els.groupsList.innerHTML = '';
    for (const group of state.groupsCache) {
      const item = document.createElement('li');
      const button = document.createElement('button');
      const id = group.group_id || group.id;

      button.textContent = `${group.group_name || group.name} (${id}) [${group.role}]`;
      button.addEventListener('click', () => {
        els.activeGroup.value = id;
        state.activeGroupRole = group.role || '';
        localStorage.setItem(CONFIG.storage.activeGroup, els.activeGroup.value);
        connectStream().catch((error) => setStatus(error.message, true));
      });

      item.appendChild(button);
      els.groupsList.appendChild(item);
    }

    setRoleFromActiveGroup();
  };

  const loadTodos = async () => {
    const groupID = (els.activeGroup.value || '').trim();
    if (!groupID) {
      return;
    }

    const data = await apiRequest(`${CONFIG.queryBase}/api/v1/todos?group_id=${encodeURIComponent(groupID)}`, { method: 'GET', headers: {} });
    state.currentTodos = data.todos || [];
    renderTodos();
  };

  const getProjectionOffset = async (groupID) => {
    const data = await apiRequest(`${CONFIG.queryBase}/api/v1/projection-offset?group_id=${encodeURIComponent(groupID)}`, { method: 'GET', headers: {} });
    return Number(data.last_event_seq || 0);
  };

  const scheduleProjectionSync = (eventSeq) => {
    if (!eventSeq || eventSeq <= 0) {
      return;
    }

    state.projection.pendingEventSeq = Math.max(state.projection.pendingEventSeq, eventSeq);
    if (state.projection.timer) {
      clearTimeout(state.projection.timer);
    }

    state.projection.timer = setTimeout(() => {
      state.projection.timer = null;
      runProjectionSync().catch((error) => setStatus(error.message, true));
    }, CONFIG.projection.debounceMs);
  };

  const runProjectionSync = async () => {
    if (state.projection.running) {
      return;
    }

    const groupID = (els.activeGroup.value || '').trim();
    if (!groupID || state.projection.pendingEventSeq <= 0) {
      return;
    }

    state.projection.running = true;
    try {
      while (state.projection.pendingEventSeq > 0) {
        const targetSeq = state.projection.pendingEventSeq;
        const offset = await getProjectionOffset(groupID);

        if (offset >= targetSeq) {
          await loadTodos();
          if (state.projection.pendingEventSeq <= targetSeq) {
            state.projection.pendingEventSeq = 0;
          }
          state.projection.delayMs = CONFIG.projection.initialDelayMs;
          continue;
        }

        await wait(state.projection.delayMs);
        state.projection.delayMs = Math.min(
          Math.floor(state.projection.delayMs * CONFIG.projection.backoffFactor),
          CONFIG.projection.maxDelayMs,
        );
      }
    } finally {
      state.projection.running = false;
      if (state.projection.pendingEventSeq > 0 && !state.projection.timer) {
        state.projection.timer = setTimeout(() => {
          state.projection.timer = null;
          runProjectionSync().catch((error) => setStatus(error.message, true));
        }, state.projection.delayMs);
      }
    }
  };

  const closeStream = () => {
    if (state.stream) {
      state.stream.close();
      state.stream = null;
    }
  };

  const connectStream = async () => {
    const groupID = (els.activeGroup.value || '').trim();
    if (!groupID) {
      setStatus('group id is required', true);
      return;
    }

    localStorage.setItem(CONFIG.storage.activeGroup, groupID);
    setRoleFromActiveGroup();
    await loadTodos();

    closeStream();

    const streamURL = `${CONFIG.queryBase}/events?group_id=${encodeURIComponent(groupID)}&token=${encodeURIComponent(state.auth.accessToken)}`;
    state.stream = new EventSource(streamURL);

    state.stream.addEventListener('datastar-patch-elements', (event) => {
      const lines = event.data.split('\n');
      const elementsLine = lines.find((line) => line.startsWith('elements '));
      if (elementsLine) {
        prependEventHTML(elementsLine.slice('elements '.length));
      }
    });

    state.stream.addEventListener('todo-event', (event) => {
      let payload = null;
      try {
        payload = JSON.parse(event.data);
      } catch (_) {
        return;
      }
      scheduleProjectionSync(Number(payload.event_seq || 0));
    });

    state.stream.onerror = async () => {
      try {
        await refreshSession();
        setTimeout(() => {
          connectStream().catch((error) => setStatus(error.message, true));
        }, CONFIG.streamRetryMs);
      } catch (_) {
        clearAuth();
        redirectToLogin();
      }
    };
  };

  const sendCommand = async (action, title, todoID) => {
    const groupID = (els.activeGroup.value || '').trim();
    if (!groupID) {
      throw new Error('group_id is required');
    }

    await apiRequest(`${CONFIG.apiBase}/api/v1/command`, {
      method: 'POST',
      body: JSON.stringify({ action, title, group_id: groupID, todo_id: todoID || '' }),
    });
  };

  const createGroup = async () => {
    const name = (els.groupName.value || '').trim();
    if (!name) {
      return;
    }

    const group = await apiRequest(`${CONFIG.apiBase}/api/v1/groups`, {
      method: 'POST',
      body: JSON.stringify({ name }),
    });

    els.groupName.value = '';
    els.activeGroup.value = group.id;
    state.activeGroupRole = 'owner';
    await loadGroups();
    await connectStream();
  };

  const addMember = async () => {
    const groupID = (els.activeGroup.value || '').trim();
    const username = (els.memberUsername.value || '').trim();
    const role = els.memberRole.value;
    if (!groupID || !username) {
      return;
    }

    await apiRequest(`${CONFIG.apiBase}/api/v1/groups/${encodeURIComponent(groupID)}/members`, {
      method: 'POST',
      body: JSON.stringify({ username, role }),
    });

    els.memberUsername.value = '';
    setStatus(`added ${username} to ${groupID} as ${role}`);
  };

  const updateMemberRole = async () => {
    const groupID = (els.activeGroup.value || '').trim();
    const username = (els.roleUsername.value || '').trim();
    const role = els.roleValue.value;
    if (!groupID || !username) {
      return;
    }

    await apiRequest(`${CONFIG.apiBase}/api/v1/groups/${encodeURIComponent(groupID)}/members/role`, {
      method: 'PATCH',
      body: JSON.stringify({ username, role }),
    });

    els.roleUsername.value = '';
    setStatus(`updated ${username} role to ${role}`);
    await loadGroups();
  };

  const createTodo = async () => {
    const title = (els.todoInput.value || '').trim();
    if (!title) {
      return;
    }

    await sendCommand('create-todo', title, '');
    els.todoInput.value = '';
  };

  const logout = async () => {
    try {
      if (state.auth.refreshToken) {
        await apiRequest(`${CONFIG.apiBase}/api/v1/auth/logout`, {
          method: 'POST',
          body: JSON.stringify({ refresh_token: state.auth.refreshToken }),
        }, false);
      }
    } finally {
      closeStream();
      clearAuth();
      redirectToLogin();
    }
  };

  const initialize = () => {
    if (!state.auth.accessToken) {
      redirectToLogin();
      return;
    }

    els.toolbarUser.textContent = state.auth.username || 'user';
    els.appShell.style.display = 'block';
    els.authGuard.style.display = 'none';
    setStatus(`Logged in as ${state.auth.username || 'user'}`);

    els.refreshBtn.addEventListener('click', () => {
      refreshSession()
        .then(() => setStatus('token refreshed'))
        .catch((error) => setStatus(error.message, true));
    });

    els.logoutBtn.addEventListener('click', () => {
      logout().catch((error) => setStatus(error.message, true));
    });

    els.createGroupBtn.addEventListener('click', () => {
      createGroup().catch((error) => setStatus(error.message, true));
    });

    els.connectGroupBtn.addEventListener('click', () => {
      connectStream().catch((error) => setStatus(error.message, true));
    });

    els.addMemberBtn.addEventListener('click', () => {
      addMember().catch((error) => setStatus(error.message, true));
    });

    els.updateRoleBtn.addEventListener('click', () => {
      updateMemberRole().catch((error) => setStatus(error.message, true));
    });

    els.sendBtn.addEventListener('click', () => {
      createTodo().catch((error) => setStatus(error.message, true));
    });

    els.todoInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') {
        createTodo().catch((error) => setStatus(error.message, true));
      }
    });

    els.feedToggle.addEventListener('change', applyFeedVisibility);

    const savedFeedVisibility = localStorage.getItem(CONFIG.storage.showFeed);
    els.feedToggle.checked = savedFeedVisibility == null ? true : savedFeedVisibility === '1';
    applyFeedVisibility();

    const savedGroup = localStorage.getItem(CONFIG.storage.activeGroup);
    if (savedGroup) {
      els.activeGroup.value = savedGroup;
    }

    loadGroups()
      .then(() => {
        if ((els.activeGroup.value || '').trim()) {
          return connectStream();
        }
        return null;
      })
      .catch((error) => setStatus(error.message, true));
  };

  initialize();
})();
