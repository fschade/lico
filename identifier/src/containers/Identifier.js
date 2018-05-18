import React, { Component } from 'react';
import { connect } from 'react-redux';
import { BrowserRouter, Switch, Route } from 'react-router-dom';

import { withStyles } from 'material-ui/styles';
import Snackbar from 'material-ui/Snackbar';
import Button from 'material-ui/Button';
import PropTypes from 'prop-types';
import renderIf from 'render-if';

import { enhanceBodyBackground } from '../utils';
import Loginscreen from '../components/Loginscreen';
import Welcomescreen from '../components/Welcomescreen';
import PrivateRoute from '../components/PrivateRoute';

// Trigger loading of background image.
enhanceBodyBackground();

const styles = () => ({
  root: {
    height: '100vh'
  }
});

class Identifier extends Component {
  render() {
    const { classes, hello, updateAvailable } = this.props;

    return (
      <div className={classes.root}>
        <BrowserRouter basename="/signin/v1">
          <Switch>
            <PrivateRoute path="/welcome" exact component={Welcomescreen} hello={hello}></PrivateRoute>
            <Route path="/" component={Loginscreen}></Route>
          </Switch>
        </BrowserRouter>
        {renderIf(updateAvailable)(() => (
          <Snackbar
            anchorOrigin={{ vertical: 'bottom', horizontal: 'left'}}
            open
            action={<Button color="accent" dense onClick={(event) => this.reload(event)}>
              Reload
            </Button>}
            SnackbarContentProps={{
              'aria-describedby': 'message-id'
            }}
            message={<span id="message-id">Update available</span>}
          />
        ))}
      </div>
    );
  }

  reload(event) {
    event.preventDefault();

    window.location.reload();
  }
}

Identifier.propTypes = {
  classes: PropTypes.object.isRequired,

  hello: PropTypes.object,
  updateAvailable: PropTypes.bool.isRequired
};

const mapStateToProps = (state) => {
  const { hello, updateAvailable } = state.common;

  return {
    hello,
    updateAvailable
  };
};

export default connect(mapStateToProps)(withStyles(styles)(Identifier));
