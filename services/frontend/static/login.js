(() => {
  'use strict';

  const CONFIG = {
    apiBase: 'http://localhost:8080',
    appURL: '/app',
    storage: {
      access: 'todo_access_token',
      legacyAccess: 'todo_token',
      refresh: 'todo_refresh_token',
      username: 'todo_username',
      userID: 'todo_user_id',
    },
  };

  const els = {
    username: document.getElementById('login-username'),
    password: document.getElementById('login-password'),
    loginBtn: document.getElementById('login-btn'),
    registerBtn: document.getElementById('register-btn'),
    status: document.getElementById('login-status'),
  };

  if (!els.loginBtn || !els.registerBtn) {
    return;
  }

  const setStatus = (message, isError = false) => {
    els.status.textContent = message;
    els.status.className = isError ? 'text-sm mt-2 text-error' : 'text-sm mt-2 text-success';
  };

  const persistAuth = (payload) => {
    const token = payload.access_token || payload.token || '';
    localStorage.setItem(CONFIG.storage.access, token);
    localStorage.setItem(CONFIG.storage.legacyAccess, token);
    localStorage.setItem(CONFIG.storage.refresh, payload.refresh_token || '');
    localStorage.setItem(CONFIG.storage.username, payload.username || '');
    localStorage.setItem(CONFIG.storage.userID, payload.user_id || '');
  };

  const authRequest = async (path) => {
    const username = (els.username.value || '').trim();
    const password = els.password.value || '';

    if (!username || !password) {
      setStatus('Username and password are required.', true);
      return;
    }

    const response = await fetch(`${CONFIG.apiBase}${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password }),
    });

    let data = null;
    try {
      data = await response.json();
    } catch (_) {
      // noop
    }

    if (!response.ok) {
      throw new Error((data && data.error) || `request failed: ${response.status}`);
    }

    persistAuth(data);
    window.location.href = CONFIG.appURL;
  };

  els.loginBtn.addEventListener('click', () => {
    authRequest('/api/v1/auth/login').catch((error) => setStatus(error.message, true));
  });

  els.registerBtn.addEventListener('click', () => {
    authRequest('/api/v1/auth/register').catch((error) => setStatus(error.message, true));
  });

  els.password.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') {
      authRequest('/api/v1/auth/login').catch((error) => setStatus(error.message, true));
    }
  });

  if (localStorage.getItem(CONFIG.storage.access)) {
    window.location.href = CONFIG.appURL;
  }
})();
