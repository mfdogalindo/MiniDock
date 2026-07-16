(() => {
  const preview = document.querySelector('[data-deployment-log-url]');
  if (!preview) return;

  const output = preview.querySelector('pre');
  const message = preview.querySelector('.muted');
  const currentStatus = preview.dataset.deploymentStatus;

  const show = text => {
    if (!text) return;
    output.textContent = text.slice(-16 * 1024);
    output.hidden = false;
    if (message) message.hidden = true;
    output.scrollTop = output.scrollHeight;
  };
  const fallback = async () => {
    try {
      const response = await fetch(preview.dataset.deploymentLogUrl, {cache: 'no-store'});
      if (response.ok) show(await response.text());
    } catch (_) {}
  };

  // EventSource receives each append from the worker without reloading the
  // application page. It also handles reconnecting after a short network loss.
  if (window.EventSource && preview.dataset.deploymentLogStreamUrl) {
    const stream = new EventSource(preview.dataset.deploymentLogStreamUrl);
    stream.addEventListener('log', event => {
      try { show(JSON.parse(event.data)); } catch (_) { show(event.data); }
    });
    stream.onerror = () => {
      stream.close();
      fallback();
      if (currentStatus === 'queued' || currentStatus === 'running') {
        setTimeout(() => window.location.reload(), 1500);
      }
    };
  } else {
    fallback();
  }
})();
