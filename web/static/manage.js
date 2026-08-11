const status = document.querySelector('#status');
const button = document.querySelector('#delete');
const publicID = location.pathname.split('/').pop();
const deleteToken = location.hash.slice(1);

if (!publicID || !deleteToken) {
  button.disabled = true;
  status.textContent = 'The delete token is missing from this link.';
}

button.addEventListener('click', async () => {
  if (!confirm('Delete this paste permanently?')) return;
  button.disabled = true;
  try {
    const response = await fetch(`/api/v1/pastes/${encodeURIComponent(publicID)}`, {
      method: 'DELETE',
      headers: {'X-Delete-Token': deleteToken},
    });
    if (!response.ok && response.status !== 404) throw new Error();
    status.textContent = 'Paste deleted or already unavailable.';
  } catch {
    status.textContent = 'Could not delete the paste. Try again.';
    button.disabled = false;
  }
});
