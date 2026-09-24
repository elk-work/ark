'use strict';
const filters = document.querySelector('#filters');
if (filters) {
  const repo = filters.dataset.repo;
  const notice = document.querySelector('#notice');
  const dialog = document.querySelector('#detail-dialog');
  const content = document.querySelector('#dialog-content');
  let filterRequest;
  const node = (tag, text, className) => {
    const el = document.createElement(tag);
    if (text !== undefined) el.textContent = text;
    if (className) el.className = className;
    return el;
  };
  const read = async (path, signal) => {
    const response = await fetch(path, { credentials: 'same-origin', signal });
    if (!response.ok) {
      if (response.status === 401) throw new Error('Your session ended. Sign in again from Repositories.');
      throw new Error((await response.json()).message || 'Could not load tasks. Try again.');
    }
    return response.json();
  };
  const age = value => {
    const hours = Math.max(0, Math.floor((Date.now() - Date.parse(value)) / 3600000));
    if (!Number.isFinite(hours)) return value;
    return hours < 1 ? 'less than an hour ago' : hours < 24 ? `${hours}h ago` : `${Math.floor(hours / 24)}d ago`;
  };
  filters.addEventListener('submit', async event => {
    event.preventDefault();
    filterRequest?.abort();
    filterRequest = new AbortController();
    const query = new URLSearchParams(new FormData(filters));
    notice.textContent = 'Loading tasks…';
    try {
      const data = await read(`/v1/repositories/${encodeURIComponent(repo)}/tasks?${query}`, filterRequest.signal);
      document.querySelectorAll('.column').forEach(column => {
        const tasks = data.tasks.filter(task => task.status === column.dataset.status);
        column.querySelector('.count').textContent = tasks.length;
        const cards = column.querySelector('.cards');
        cards.replaceChildren();
        tasks.forEach(task => {
          const card = node('a', undefined, 'card');
          card.href = `/ui/${encodeURIComponent(repo)}?task=${encodeURIComponent(task.id)}`;
          card.dataset.task = task.id; card.title = task.id;
          card.append(node('span', `#${task.number}`, 'number'), node('h3', task.title), node('p', `${task.actor.name} · ${age(task.created_at)}`, 'muted'));
          if (task.elk_ref) card.append(node('p', `Elk ${task.elk_ref}`, 'parent'));
          cards.append(card);
        });
        if (!tasks.length) cards.append(node('p', 'No tasks', 'empty'));
      });
      document.querySelector('#task-detail')?.remove();
      history.replaceState(null, '', `?${query}`);
      notice.textContent = `${data.tasks.length} ${data.tasks.length === 1 ? "task" : "tasks"} shown.`;
    } catch (error) { if (error.name !== 'AbortError') notice.textContent = error.message; }
  });
  document.querySelector('.board').addEventListener('click', async event => {
    const card = event.target.closest('a[data-task]');
    if (!card || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
    event.preventDefault(); notice.textContent = 'Loading task…';
    try {
      const data = await read(`/v1/repositories/${encodeURIComponent(repo)}/tasks/${encodeURIComponent(card.dataset.task)}`);
      const task = data.task;
      const title = node('h2', task.title); title.id = 'dialog-title';
      content.replaceChildren(node('p', `TASK #${task.number} · ${task.status.replaceAll('_', ' ')}`, 'eyebrow'), title, node('code', task.id), node('p', `${task.actor.name} · Created ${task.created_at} · Updated ${task.updated_at}`, 'muted'));
      if (task.elk_ref) {
        const parent = node('p', 'Elk parent: ');
        if (task.elk_url) {
          const url = new URL(task.elk_url);
          if (url.origin === 'https://elk.work' && /^\/open\/[A-Za-z0-9_-]+$/.test(url.pathname)) {
            const link = node('a', task.elk_ref); link.href = url.href; parent.append(link);
          } else parent.append(document.createTextNode(task.elk_ref));
        } else parent.append(document.createTextNode(task.elk_ref));
        content.append(parent);
      }
      content.append(node('h3', 'Body'), node('pre', task.body || 'No description.'));
      const groups = [
        ['Comments', data.comments, item => [item.body, `${item.created_by} · ${item.created_at}`, item.supersedes_id ? `Corrects ${item.supersedes_id}` : '']],
        ['Runs', data.runs, item => [`${item.agent_name} · ${item.status}`, item.input_summary, item.result_summary, `${item.started_at || ''} — ${item.finished_at || ''}`]],
        ['Pull requests', data.pull_requests, item => [`#${item.number} ${item.title} · ${item.status}`, item.body]],
        ['Artifacts', data.artifacts, item => [item.name, `${item.media_type} · ${item.size_bytes} bytes`, item.sha256]]
      ];
      groups.forEach(([label, items, describe]) => {
        content.append(node('h3', label));
        if (!items.length) content.append(node('p', `No ${label.toLowerCase()}`, 'empty'));
        items.forEach(item => {
          const article = node('article');
          describe(item).filter(Boolean).forEach(text => article.append(node('pre', text)));
          article.append(node('code', item.id)); content.append(article);
        });
      });
      notice.textContent = ''; dialog.showModal();
    } catch (error) { notice.textContent = error.message; }
  });
}
