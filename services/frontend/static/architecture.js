(() => {
  'use strict';

  const STORAGE = {
    access: 'todo_access_token',
    legacyAccess: 'todo_token',
    refresh: 'todo_refresh_token',
    username: 'todo_username',
    userID: 'todo_user_id',
  };

  const token = localStorage.getItem(STORAGE.access) || localStorage.getItem(STORAGE.legacyAccess);
  if (!token) {
    window.location.href = '/login';
    return;
  }

  const toolbarUser = document.getElementById('toolbar-user');
  const shell = document.getElementById('architecture-shell');
  const guard = document.getElementById('auth-guard');
  const logoutBtn = document.getElementById('logout-btn');

  if (!toolbarUser || !shell || !guard || !logoutBtn) {
    return;
  }

  toolbarUser.textContent = localStorage.getItem(STORAGE.username) || 'user';
  shell.style.display = 'block';
  guard.style.display = 'none';

  logoutBtn.addEventListener('click', () => {
    localStorage.removeItem(STORAGE.access);
    localStorage.removeItem(STORAGE.legacyAccess);
    localStorage.removeItem(STORAGE.refresh);
    localStorage.removeItem(STORAGE.username);
    localStorage.removeItem(STORAGE.userID);
    window.location.href = '/login';
  });
})();
