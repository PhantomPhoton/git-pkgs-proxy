// The first click arms a button and the second deletes, so a stray click
// removes nothing.
document.querySelectorAll('.cache-delete').forEach(button => {
    button.addEventListener('click', async () => {
        if (button.dataset.armed !== 'true') {
            document.querySelectorAll('.cache-delete').forEach(other => {
                other.dataset.armed = 'false';
                other.textContent = other.dataset.label;
            });
            button.dataset.armed = 'true';
            button.textContent = 'Click again to delete';
            return;
        }
        const errorBox = document.getElementById('cache-delete-error');
        let url = button.closest('[data-cache-url]').dataset.cacheUrl;
        if (button.dataset.filename) {
            url += '?filename=' + encodeURIComponent(button.dataset.filename);
        }
        button.disabled = true;
        button.textContent = 'Deleting…';
        try {
            const resp = await fetch(url, { method: 'DELETE' });
            if (!resp.ok) {
                const body = await resp.json().catch(() => ({}));
                throw new Error(body.message || `HTTP ${resp.status}`);
            }
            window.location.reload();
        } catch (err) {
            errorBox.textContent = 'Delete failed: ' + err.message;
            errorBox.classList.remove('hidden');
            button.disabled = false;
            button.dataset.armed = 'false';
            button.textContent = button.dataset.label;
        }
    });
});
