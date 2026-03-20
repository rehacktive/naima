const serverUrlEl = document.getElementById('serverUrl');
const apiTokenEl = document.getElementById('apiToken');
const topicSelectEl = document.getElementById('topicSelect');
const newTopicWrapEl = document.getElementById('newTopicWrap');
const newTopicEl = document.getElementById('newTopic');
const notifyTelegramEl = document.getElementById('notifyTelegram');
const refreshTopicsEl = document.getElementById('refreshTopics');
const ingestBtnEl = document.getElementById('ingestBtn');
const statusEl = document.getElementById('status');

const storageKeys = {
  serverUrl: 'naima_server_url',
  apiToken: 'naima_api_token',
  notifyTelegram: 'naima_notify_telegram',
  topicMode: 'naima_topic_mode',
  topicId: 'naima_topic_id',
  newTopic: 'naima_new_topic'
};

function setStatus(text) {
  statusEl.textContent = text || '';
}

function normalizeServerUrl(raw) {
  const trimmed = String(raw || '').trim();
  if (!trimmed) {
    return 'http://localhost:8080';
  }
  return trimmed.replace(/\/$/, '');
}

function authHeaders(token) {
  return {
    'Authorization': 'Bearer ' + token,
    'Content-Type': 'application/json'
  };
}

async function saveSettings() {
  await chrome.storage.sync.set({
    [storageKeys.serverUrl]: serverUrlEl.value.trim(),
    [storageKeys.apiToken]: apiTokenEl.value.trim(),
    [storageKeys.notifyTelegram]: notifyTelegramEl.checked,
    [storageKeys.topicMode]: topicSelectEl.value,
    [storageKeys.newTopic]: newTopicEl.value.trim()
  });
}

async function loadSettings() {
  const values = await chrome.storage.sync.get({
    [storageKeys.serverUrl]: 'http://localhost:8080',
    [storageKeys.apiToken]: '',
    [storageKeys.notifyTelegram]: true,
    [storageKeys.topicMode]: '',
    [storageKeys.newTopic]: ''
  });
  serverUrlEl.value = values[storageKeys.serverUrl] || 'http://localhost:8080';
  apiTokenEl.value = values[storageKeys.apiToken] || '';
  notifyTelegramEl.checked = Boolean(values[storageKeys.notifyTelegram]);
  newTopicEl.value = values[storageKeys.newTopic] || '';
}

function updateTopicMode() {
  const isNew = topicSelectEl.value === '__new__';
  newTopicWrapEl.classList.toggle('hidden', !isNew);
}

async function fetchTopics() {
  const token = apiTokenEl.value.trim();
  const serverUrl = normalizeServerUrl(serverUrlEl.value);
  if (!token) {
    setStatus('Set API token first.');
    return;
  }

  refreshTopicsEl.disabled = true;
  setStatus('Loading topics...');
  try {
    const resp = await fetch(serverUrl + '/api/pkb/graph', {
      headers: authHeaders(token)
    });
    if (!resp.ok) {
      throw new Error(await resp.text());
    }
    const payload = await resp.json();
    const topics = Array.isArray(payload.topics) ? payload.topics : [];
    const previous = topicSelectEl.value;
    topicSelectEl.innerHTML = '<option value="">Select topic...</option><option value="__new__">Create new topic</option>';
    for (const topic of topics) {
      const option = document.createElement('option');
      option.value = String(topic.id);
      option.textContent = topic.title || ('Topic ' + topic.id);
      topicSelectEl.appendChild(option);
    }
    if ([...topicSelectEl.options].some((o) => o.value === previous)) {
      topicSelectEl.value = previous;
    }
    updateTopicMode();
    setStatus('Topics loaded.');
  } catch (err) {
    setStatus('Load topics failed: ' + (err?.message || String(err)));
  } finally {
    refreshTopicsEl.disabled = false;
  }
}

async function currentTabUrl() {
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  const tab = tabs && tabs[0];
  const url = tab && tab.url ? String(tab.url) : '';
  if (!/^https?:\/\//i.test(url)) {
    throw new Error('Current tab has no HTTP/HTTPS URL.');
  }
  return url;
}

async function ingestCurrentTab() {
  const token = apiTokenEl.value.trim();
  const serverUrl = normalizeServerUrl(serverUrlEl.value);
  const topicValue = topicSelectEl.value;
  const newTopic = newTopicEl.value.trim();
  if (!token) {
    setStatus('Set API token first.');
    return;
  }
  if (!topicValue) {
    setStatus('Select a topic or create a new one.');
    return;
  }
  if (topicValue === '__new__' && !newTopic) {
    setStatus('New topic is required.');
    return;
  }

  ingestBtnEl.disabled = true;
  refreshTopicsEl.disabled = true;
  setStatus('Reading current tab...');
  try {
    const url = await currentTabUrl();
    const payload = {
      url,
      notify_telegram: notifyTelegramEl.checked
    };
    if (topicValue === '__new__') {
      payload.new_topic = newTopic;
    } else {
      payload.topic_id = Number(topicValue);
    }

    setStatus('Sending URL to Naima...');
    const resp = await fetch(serverUrl + '/api/pkb/ingest', {
      method: 'POST',
      headers: authHeaders(token),
      body: JSON.stringify(payload)
    });
    if (!resp.ok) {
      throw new Error(await resp.text());
    }
    const result = await resp.json();
    const title = result?.document?.title || 'document';
    const method = result?.ingest_method || 'unknown';
    const suffix = notifyTelegramEl.checked ? ' Telegram notification requested.' : '';
    setStatus('Ingested: ' + title + ' via ' + method + '.' + suffix);
    await saveSettings();
  } catch (err) {
    setStatus('Ingest failed: ' + (err?.message || String(err)));
  } finally {
    ingestBtnEl.disabled = false;
    refreshTopicsEl.disabled = false;
  }
}

serverUrlEl.addEventListener('change', async () => {
  await saveSettings();
});
apiTokenEl.addEventListener('change', async () => {
  await saveSettings();
});
notifyTelegramEl.addEventListener('change', async () => {
  await saveSettings();
});
topicSelectEl.addEventListener('change', async () => {
  updateTopicMode();
  await saveSettings();
});
newTopicEl.addEventListener('change', async () => {
  await saveSettings();
});
refreshTopicsEl.addEventListener('click', fetchTopics);
ingestBtnEl.addEventListener('click', ingestCurrentTab);

(async () => {
  await loadSettings();
  updateTopicMode();
  if (apiTokenEl.value.trim()) {
    await fetchTopics();
  }
})();
